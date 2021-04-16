package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" // Register pprof handlers with DefaultServeMux.
	"os"
	"runtime"
	"strconv"

	auth "github.com/abbot/go-http-auth"
	"github.com/buchgr/bazel-remote/cache"
	"github.com/buchgr/bazel-remote/cache/disk"

	"github.com/buchgr/bazel-remote/config"
	"github.com/buchgr/bazel-remote/server"
	"github.com/buchgr/bazel-remote/utils/flags"
	"github.com/buchgr/bazel-remote/utils/idle"
	"github.com/buchgr/bazel-remote/utils/rlimit"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpmetrics "github.com/slok/go-http-metrics/metrics/prometheus"
	middleware "github.com/slok/go-http-metrics/middleware"
	middlewarestd "github.com/slok/go-http-metrics/middleware/std"
	"github.com/urfave/cli/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	logFlags = log.Ldate | log.Ltime | log.LUTC
)

// gitCommit is the version stamp for the server. The value of this var
// is set through linker options.
var gitCommit string

func main() {
	log.SetFlags(logFlags)

	maybeGitCommitMsg := ""
	if len(gitCommit) > 0 && gitCommit != "{STABLE_GIT_COMMIT}" {
		maybeGitCommitMsg = fmt.Sprintf(" from git commit %s", gitCommit)
	}
	log.Printf("bazel-remote built with %s%s.",
		runtime.Version(), maybeGitCommitMsg)

	app := cli.NewApp()

	cli.AppHelpTemplate = flags.Template
	cli.HelpPrinterCustom = flags.HelpPrinter
	// Force the use of cli.HelpPrinterCustom.
	app.ExtraInfo = func() map[string]string { return map[string]string{} }

	app.Flags = flags.GetCliFlags()
	app.Action = run

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal("bazel-remote terminated:", err)
	}
}

func run(ctx *cli.Context) error {
	configFile := ctx.String("config_file")
	var c *config.Config
	var err error
	if configFile != "" {
		c, err = config.NewFromYamlFile(configFile)
	} else {
		var s3 *config.S3CloudStorageConfig
		if ctx.String("s3.bucket") != "" {
			s3 = &config.S3CloudStorageConfig{
				Endpoint:        ctx.String("s3.endpoint"),
				Bucket:          ctx.String("s3.bucket"),
				Prefix:          ctx.String("s3.prefix"),
				AccessKeyID:     ctx.String("s3.access_key_id"),
				SecretAccessKey: ctx.String("s3.secret_access_key"),
				DisableSSL:      ctx.Bool("s3.disable_ssl"),
				IAMRoleEndpoint: ctx.String("s3.iam_role_endpoint"),
				Region:          ctx.String("s3.region"),
			}
		}

		var hc *config.HTTPBackendConfig
		if ctx.String("http_proxy.url") != "" {
			hc = &config.HTTPBackendConfig{
				BaseURL: ctx.String("http_proxy.url"),
			}
		}

		var gcs *config.GoogleCloudStorageConfig
		if ctx.String("gcs_proxy.bucket") != "" {
			gcs = &config.GoogleCloudStorageConfig{
				Bucket:                ctx.String("gcs_proxy.bucket"),
				UseDefaultCredentials: ctx.Bool("gcs_proxy.use_default_credentials"),
				JSONCredentialsFile:   ctx.String("gcs_proxy.json_credentials_file"),
			}
		}

		c, err = config.New(
			ctx.String("dir"),
			ctx.Int("max_size"),
			ctx.String("storage_mode"),
			ctx.String("host"),
			ctx.Int("port"),
			ctx.Int("grpc_port"),
			ctx.String("profile_host"),
			ctx.Int("profile_port"),
			ctx.String("htpasswd_file"),
			ctx.Int("max_queued_uploads"),
			ctx.Int("num_uploaders"),
			ctx.String("tls_ca_file"),
			ctx.String("tls_cert_file"),
			ctx.String("tls_key_file"),
			ctx.Duration("idle_timeout"),
			hc,
			gcs,
			s3,
			ctx.Bool("disable_http_ac_validation"),
			ctx.Bool("disable_grpc_ac_deps_check"),
			ctx.Bool("enable_ac_key_instance_mangling"),
			ctx.Bool("enable_endpoint_metrics"),
			ctx.Bool("experimental_remote_asset_api"),
			ctx.Duration("http_read_timeout"),
			ctx.Duration("http_write_timeout"),
		)
	}

	if err != nil {
		fmt.Fprintf(ctx.App.Writer, "%v\n\n", err)
		cli.ShowAppHelp(ctx)
		return cli.Exit("", 1)
	}

	if ctx.NArg() > 0 {
		fmt.Fprintf(ctx.App.Writer,
			"Error: bazel-remote does not take positional aguments\n")
		for i := 0; i < ctx.NArg(); i++ {
			fmt.Fprintf(ctx.App.Writer, "arg: %s\n", ctx.Args().Get(i))
		}
		fmt.Fprintf(ctx.App.Writer, "\n")

		cli.ShowAppHelp(ctx)
		return cli.Exit("", 1)
	}

	rlimit.Raise()

	accessLogger := log.New(os.Stdout, "", logFlags)
	errorLogger := log.New(os.Stderr, "", logFlags)

	proxyCache, err := c.GetProxy(accessLogger, errorLogger)
	if err != nil {
		log.Fatal(err)
	}

	diskCache, err := disk.New(c.Dir, int64(c.MaxSize)*1024*1024*1024, c.StorageMode, proxyCache)
	if err != nil {
		log.Fatal(err)
	}

	tlsConfig, err := c.GetTLSConfig()
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:         c.Host + ":" + strconv.Itoa(c.Port),
		Handler:      mux,
		ReadTimeout:  c.HTTPReadTimeout,
		TLSConfig:    tlsConfig,
		WriteTimeout: c.HTTPWriteTimeout,
	}

	validateAC := !c.DisableHTTPACValidation
	h := server.NewHTTPCache(diskCache, accessLogger, errorLogger, validateAC, c.EnableACKeyInstanceMangling, gitCommit)

	var htpasswdSecrets auth.SecretProvider
	cacheHandler := h.CacheHandler
	if c.HtpasswdFile != "" {
		htpasswdSecrets = auth.HtpasswdFileProvider(c.HtpasswdFile)
		cacheHandler = wrapAuthHandler(cacheHandler, htpasswdSecrets, c.Host)
	}

	var idleTimer *idle.Timer
	if c.IdleTimeout > 0 {
		idleTimer = idle.NewTimer(c.IdleTimeout)
		cacheHandler = wrapIdleHandler(cacheHandler, idleTimer, accessLogger, httpServer)
	}

	acKeyManglingStatus := "disabled"
	if c.EnableACKeyInstanceMangling {
		acKeyManglingStatus = "enabled"
	}
	log.Println("Mangling non-empty instance names with AC keys:", acKeyManglingStatus)

	if c.EnableEndpointMetrics {
		metricsMdlw := middleware.New(middleware.Config{
			Recorder: httpmetrics.NewRecorder(httpmetrics.Config{
				DurationBuckets: c.MetricsDurationBuckets,
			}),
		})
		mux.Handle("/metrics", middlewarestd.Handler("metrics", metricsMdlw, promhttp.Handler()))
		mux.Handle("/status", middlewarestd.Handler("status", metricsMdlw, http.HandlerFunc(h.StatusPageHandler)))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			middlewarestd.Handler(r.Method, metricsMdlw, http.HandlerFunc(cacheHandler)).ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("/status", h.StatusPageHandler)
		mux.HandleFunc("/", cacheHandler)
	}

	if c.GRPCPort > 0 {

		if c.GRPCPort == c.Port {
			log.Fatalf("Error: gRPC and HTTP ports (%d) conflict", c.Port)
		}

		go func() {
			addr := c.Host + ":" + strconv.Itoa(c.GRPCPort)

			opts := []grpc.ServerOption{}
			streamInterceptors := []grpc.StreamServerInterceptor{}
			unaryInterceptors := []grpc.UnaryServerInterceptor{}

			if c.EnableEndpointMetrics {
				streamInterceptors = append(streamInterceptors, grpc_prometheus.StreamServerInterceptor)
				unaryInterceptors = append(unaryInterceptors, grpc_prometheus.UnaryServerInterceptor)
				grpc_prometheus.EnableHandlingTimeHistogram(grpc_prometheus.WithHistogramBuckets(c.MetricsDurationBuckets))
			}

			if tlsConfig != nil {
				opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig)))
			}

			if htpasswdSecrets != nil {
				gba := server.NewGrpcBasicAuth(htpasswdSecrets)
				streamInterceptors = append(streamInterceptors, gba.StreamServerInterceptor)
				unaryInterceptors = append(unaryInterceptors, gba.UnaryServerInterceptor)
			}

			if idleTimer != nil {
				it := server.NewGrpcIdleTimer(idleTimer)
				streamInterceptors = append(streamInterceptors, it.StreamServerInterceptor)
				unaryInterceptors = append(unaryInterceptors, it.UnaryServerInterceptor)
			}

			opts = append(opts, grpc.ChainStreamInterceptor(streamInterceptors...))
			opts = append(opts, grpc.ChainUnaryInterceptor(unaryInterceptors...))

			log.Printf("Starting gRPC server on address %s", addr)

			validateAC := !c.DisableGRPCACDepsCheck
			validateStatus := "disabled"
			if validateAC {
				validateStatus = "enabled"
			}
			log.Println("gRPC AC dependency checks:", validateStatus)

			enableRemoteAssetAPI := c.ExperimentalRemoteAssetAPI
			remoteAssetStatus := "disabled"
			if enableRemoteAssetAPI {
				remoteAssetStatus = "enabled"
			}
			log.Println("experimental gRPC remote asset API:", remoteAssetStatus)

			err3 := server.ListenAndServeGRPC(addr, opts,
				validateAC,
				c.EnableACKeyInstanceMangling,
				enableRemoteAssetAPI,
				diskCache, accessLogger, errorLogger)
			if err3 != nil {
				log.Fatal(err3)
			}
		}()
	}

	if c.ProfilePort > 0 {
		go func() {
			// Allow access to /debug/pprof/ URLs.
			profileAddr := c.ProfileHost + ":" +
				strconv.Itoa(c.ProfilePort)
			log.Printf("Starting HTTP server for profiling on address %s",
				profileAddr)
			log.Fatal(http.ListenAndServe(profileAddr, nil))
		}()
	}

	validateStatus := "disabled"
	if validateAC {
		validateStatus = "enabled"
	}

	if len(c.TLSCertFile) > 0 && len(c.TLSKeyFile) > 0 {
		log.Printf("Starting HTTPS server on address %s", httpServer.Addr)
		log.Println("HTTP AC validation:", validateStatus)
		return httpServer.ListenAndServeTLS(c.TLSCertFile, c.TLSKeyFile)
	}

	if idleTimer != nil {
		log.Printf("Starting idle timer with value %v", c.IdleTimeout)
		idleTimer.Start()
	}

	log.Printf("Starting HTTP server on address %s", httpServer.Addr)
	log.Println("HTTP AC validation:", validateStatus)
	return httpServer.ListenAndServe()
}

func wrapIdleHandler(handler http.HandlerFunc, idleTimer *idle.Timer, accessLogger cache.Logger, httpServer *http.Server) http.HandlerFunc {

	tearDown := make(chan struct{})
	idleTimer.Register(tearDown)

	go func() {
		<-tearDown
		accessLogger.Printf("Shutting down after idle timeout")
		httpServer.Shutdown(context.Background())
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idleTimer.ResetTimer()
		handler(w, r)
	})
}

func wrapAuthHandler(handler http.HandlerFunc, secrets auth.SecretProvider, host string) http.HandlerFunc {
	authenticator := auth.NewBasicAuthenticator(host, secrets)
	return auth.JustCheck(authenticator, handler)
}
