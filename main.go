package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	log "github.com/sirupsen/logrus"
	"github.com/sourcegraph/conc/pool"
	"github.com/spf13/viper"
	cache "github.com/victorspringer/http-cache"
	"github.com/victorspringer/http-cache/adapter/memory"
	"github.com/victorspringer/http-cache/adapter/redis"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Attempts int = iota
	Retry
)

const (
	keepaliveTimeout = 2 * time.Minute
)

var (
	//go:embed data
	dataFs embed.FS
)

// Backend holds the data about a server
type Backend struct {
	Name         string
	URL          *url.URL
	Alive        bool
	mux          sync.RWMutex
	ReverseProxy *httputil.ReverseProxy
}

// Check is a combined SetAlive and isBackendAlive check
func (b *Backend) Check() bool {
	alive := isBackendAlive(b.URL)
	b.SetAlive(alive)
	return alive
}

// SetAlive for this backend
func (b *Backend) SetAlive(alive bool) {
	b.mux.Lock()
	b.Alive = alive
	b.mux.Unlock()
}

// IsAlive returns true when backend is alive
func (b *Backend) IsAlive() (alive bool) {
	b.mux.RLock()
	alive = b.Alive
	b.mux.RUnlock()
	return
}

// ServerPool holds information about reachable backends
type ServerPool struct {
	backends []*Backend
	current  uint64
}

// AddBackend to the server pool
func (s *ServerPool) AddBackend(backend *Backend) {
	s.backends = append(s.backends, backend)
}

// NextIndex atomically increase the counter and return an index
func (s *ServerPool) NextIndex() int {
	return int(atomic.AddUint64(&s.current, uint64(1)) % uint64(len(s.backends)))
}

// MarkBackendStatus changes a status of a backend
func (s *ServerPool) MarkBackendStatus(backendUrl *url.URL, alive bool) {
	for _, b := range s.backends {
		if b.URL.String() == backendUrl.String() {
			b.SetAlive(alive)
			break
		}
	}
}

// GetNextPeer returns next active peer to take a connection
func (s *ServerPool) GetNextPeer() *Backend {
	// loop entire backends to find out an Alive backend
	next := s.NextIndex()
	l := len(s.backends) + next // start from next and move a full cycle
	for i := next; i < l; i++ {
		idx := i % len(s.backends)     // take an index by modding
		if s.backends[idx].IsAlive() { // if we have an alive backend, use it and store if its not the original one
			if i != next {
				atomic.StoreUint64(&s.current, uint64(idx))
			}
			return s.backends[idx]
		}
	}
	return nil
}

// HealthCheck pings the backends and update the status
func (s *ServerPool) HealthCheck() {
	checkPool := pool.New()

	checkBackend := func(b *Backend) func() {
		return func() {
			lastStatus := b.IsAlive()

			alive := b.Check()

			if alive != lastStatus {
				log.WithFields(log.Fields{
					"backend": b.Name,
					"alive":   alive,
				}).Info("Backend status changed")
			}
		}
	}

	for _, b := range s.backends {
		checkPool.Go(checkBackend(b))
	}

	checkPool.Wait()
}

// GetAttemptsFromContext returns the attempts for request
func GetAttemptsFromContext(r *http.Request) int {
	if attempts, ok := r.Context().Value(Attempts).(int); ok {
		return attempts
	}
	return 1
}

// GetAttemptsFromContext returns the attempts for request
func GetRetryFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value(Retry).(int); ok {
		return retry
	}
	return 0
}

// lb load balances the incoming request
func lb(w http.ResponseWriter, r *http.Request) {
	attempts := GetAttemptsFromContext(r)
	if attempts > 3 {
		log.Printf("%s(%s) Max attempts reached, terminating\n", r.RemoteAddr, r.URL.Path)
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

	peer := serverPool.GetNextPeer()

	if peer != nil {
		peer.ReverseProxy.ServeHTTP(w, r)
		return
	}

	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

// isAlive checks whether a backend is Alive by establishing a TCP connection
func isBackendAlive(u *url.URL) bool {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(u.String(), "/")+"/v1/status", nil)

	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"url":   u.String(),
		}).Error("Error with healthcheck request")
		return false
	}

	res, err := healthCheckClient.Do(req)

	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"url":   u.String(),
		}).Debug("Health check failed")
		return false
	}

	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()

	if res.StatusCode != http.StatusOK {
		log.WithFields(log.Fields{
			"error": err,
			"url":   u.String(),
			"code":  res.StatusCode,
		}).Debug("Health check returned invalid response code")
		return false
	}

	return true
}

// healthCheck runs a routine for check status of the backends every 2 mins
func healthCheck() {
	t := time.NewTicker(time.Minute)
	for {
		log.Info("Starting health check...")
		serverPool.HealthCheck()
		log.Info("Health check completed.")

		<-t.C
	}
}

var (
	serverPool        *ServerPool
	healthCheckClient *http.Client
)

type BackendConfig struct {
	Name string `mapstructure:"name"`
	URL  string `mapstructure:"url""`
}

func main() {
	var port int
	flag.IntVar(&port, "port", 3030, "Port to serve")
	flag.Parse()

	healthCheckClient = &http.Client{
		Timeout: keepaliveTimeout,
	}

	backends := make([]BackendConfig, 0)

	viper.SetDefault("cache.driver", "memory")

	viper.AddConfigPath("/etc")
	viper.AddConfigPath("/config")
	viper.AddConfigPath(".")
	viper.SetConfigName("owapibalance")
	viper.SetConfigType("yaml")

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalln("Unable to read config", err)
	}

	if err := viper.UnmarshalKey("backends", &backends); err != nil {
		log.Fatalln("Unable to unmarshal config", err)
	}

	log.WithField("backends", len(backends)).Info("Loaded backends and configuration")

	serverPool = &ServerPool{}

	for _, b := range backends {
		serverUrl, err := url.Parse(b.URL)

		if err != nil {
			log.WithFields(log.Fields{
				"error":  err,
				"server": b.URL,
			}).Fatal("Unable to parse server url")
		}

		proxy := httputil.NewSingleHostReverseProxy(serverUrl)

		proxy.ModifyResponse = func(r *http.Response) error {
			r.Header.Add("X-API-Server", b.Name)
			return nil
		}

		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, e error) {
			log.WithFields(log.Fields{
				"error": e,
				"host":  serverUrl.Host,
			}).Debug("Proxy returned error")

			retries := GetRetryFromContext(request)
			if retries < 3 {
				select {
				case <-request.Context().Done():
					// Exit out
				case <-time.After(10 * time.Millisecond):
					ctx := context.WithValue(request.Context(), Retry, retries+1)
					proxy.ServeHTTP(writer, request.WithContext(ctx))
				}
				return
			}

			// after 3 retries, mark this backend as down
			serverPool.MarkBackendStatus(serverUrl, false)

			// if the same request routing for few attempts with different backends, increase the count
			attempts := GetAttemptsFromContext(request)

			log.WithFields(log.Fields{
				"addr":     request.RemoteAddr,
				"attempts": attempts,
				"path":     request.URL.Path,
			}).Debug("Attempting retry")

			ctx := context.WithValue(request.Context(), Attempts, attempts+1)
			lb(writer, request.WithContext(ctx))
		}

		serverPool.AddBackend(&Backend{
			Name:         b.Name,
			URL:          serverUrl,
			Alive:        true,
			ReverseProxy: proxy,
		})

		log.WithFields(log.Fields{
			"name": b.Name,
			"url":  serverUrl,
		}).Info("Configured server")
	}

	var adapter cache.Adapter

	switch viper.GetString("cache.driver") {
	case "redis":
		ringOpt := &redis.RingOptions{
			Addrs: viper.GetStringMapString("cache.servers"),
		}

		adapter = redis.NewAdapter(ringOpt)
	default:
		var err error

		adapter, err = memory.NewAdapter(
			memory.AdapterWithAlgorithm(memory.LRU),
			memory.AdapterWithCapacity(10000000),
		)

		if err != nil {
			log.Fatalln("Memory adapter error:", err)
		}
	}

	cacheClient, err := cache.NewClient(
		cache.ClientWithAdapter(adapter),
		cache.ClientWithTTL(2*time.Minute),
		cache.ClientWithRefreshKey("opn"),
	)

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)

	r.Group(func(r chi.Router) {
		r.Use(cacheClient.Middleware)

		r.Get("/v1", lb)
		r.Get("/v2", lb)
		r.Get("/v3", lb)
	})

	rootFs, err := fs.Sub(dataFs, "data")

	if err != nil {
		log.WithError(err).Fatal("Unable to use docs directory as sub filesystem")
	}

	httpFs := http.FileServer(http.FS(rootFs))

	r.Get("/docs/*", httpFs.ServeHTTP)
	r.Get("/style.css", httpFs.ServeHTTP)
	r.Get("/index.html", httpFs.ServeHTTP)
	r.Get("/", httpFs.ServeHTTP)

	// start health checking
	go healthCheck()

	log.Printf("Load Balancer started at :%d\n", port)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), r); err != nil {
		log.WithError(err).Fatal("Unable to start http server")
	}
}
