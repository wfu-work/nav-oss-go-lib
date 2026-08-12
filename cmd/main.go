package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wfu-work/nav-oss-go-lib/server"
	"github.com/wfu-work/nav-oss-go-lib/webs"
)

func main() {
	settings, err := parseOptions()
	if err != nil {
		log.Fatalf("load nav-oss configuration: %v", err)
	}

	management, err := server.New(server.Config{
		StateFile:         settings.StateFile,
		RuntimeDir:        settings.RuntimeDir,
		AdminUsername:     settings.AdminUsername,
		AdminPassword:     settings.AdminPassword,
		AdminPasswordHash: settings.AdminPasswordHash,
		SessionTTL:        settings.SessionTTL,
		APIAddress:        settings.APIAddr,
		ConsoleAddress:    settings.ConsoleAddr,
	})
	if err != nil {
		log.Fatalf("initialize nav-oss management service: %v", err)
	}
	defer func() {
		if closeErr := management.Close(); closeErr != nil {
			log.Printf("close nav-oss providers: %v", closeErr)
		}
	}()

	webHandler, err := webs.NewHandler()
	if err != nil {
		log.Fatalf("load nav-oss web application: %v", err)
	}
	apiTarget, err := url.Parse(displayURL(settings.APIAddr))
	if err != nil {
		log.Fatalf("resolve nav-oss API address: %v", err)
	}
	apiProxy := httputil.NewSingleHostReverseProxy(apiTarget)
	apiProxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, proxyErr error) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"code": http.StatusBadGateway,
			"msg":  "nav-oss API 暂不可用",
			"data": nil,
		})
		log.Printf("proxy nav-oss API: %v", proxyErr)
	}

	consoleMux := http.NewServeMux()
	consoleMux.Handle("/api/", apiProxy)
	consoleMux.Handle("/", webHandler)

	apiServer := newHTTPServer(settings.APIAddr, management.Handler())
	consoleServer := newHTTPServer(settings.ConsoleAddr, consoleMux)
	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errChannel := make(chan error, 2)
	go serve("API", apiServer, errChannel)
	go serve("Console", consoleServer, errChannel)

	log.Printf("nav-oss API is available at %s", displayURL(settings.APIAddr))
	log.Printf("nav-oss Console is available at %s", displayURL(settings.ConsoleAddr))

	select {
	case <-shutdownContext.Done():
	case serveErr := <-errChannel:
		if serveErr != nil {
			log.Printf("nav-oss server stopped unexpectedly: %v", serveErr)
		}
		stop()
	}

	timeoutContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = errors.Join(apiServer.Shutdown(timeoutContext), consoleServer.Shutdown(timeoutContext)); err != nil {
		log.Printf("shutdown nav-oss servers: %v", err)
	}
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

func serve(name string, httpServer *http.Server, errChannel chan<- error) {
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errChannel <- fmt.Errorf("serve nav-oss %s: %w", name, err)
		return
	}
	errChannel <- nil
}

func parseOptions() (options, error) {
	configFlag := flag.String("config", "", "JSON configuration file")
	apiFlag := flag.String("api-addr", "", "nav-oss API listen address")
	consoleFlag := flag.String("console-addr", "", "nav-oss Console listen address")
	legacyAddressFlag := flag.String("addr", "", "deprecated alias for -console-addr")
	stateFlag := flag.String("state-file", "", "persistent console state file")
	runtimeFlag := flag.String("runtime-dir", "", "managed object-storage runtime directory")
	usernameFlag := flag.String("admin-username", "", "console administrator username")
	sessionFlag := flag.Int("session-ttl-minutes", 0, "console session lifetime in minutes")
	flag.Parse()

	visited := make(map[string]bool)
	flag.Visit(func(item *flag.Flag) { visited[item.Name] = true })
	configPath := firstNonEmpty(valueIf(visited["config"], *configFlag), os.Getenv("NAV_OSS_CONFIG"))
	configuration, err := loadFileConfig(configPath)
	if err != nil {
		return options{}, err
	}

	apiAddress := resolveOption(visited["api-addr"], *apiFlag, "NAV_OSS_API_ADDR", configuration.APIAddr, "127.0.0.1:9000")
	consoleAddress := resolveOption(visited["console-addr"], *consoleFlag, "NAV_OSS_CONSOLE_ADDR", configuration.ConsoleAddr, "127.0.0.1:9001")
	if visited["addr"] {
		consoleAddress = *legacyAddressFlag
	}
	stateFile := resolveOption(visited["state-file"], *stateFlag, "NAV_OSS_STATE_FILE", configuration.StateFile, filepath.Join("data", "nav-oss-state.json"))
	runtimeDir := resolveOption(visited["runtime-dir"], *runtimeFlag, "NAV_OSS_RUNTIME_DIR", configuration.RuntimeDir, filepath.Join(filepath.Dir(stateFile), "runtimes"))
	username := resolveOption(visited["admin-username"], *usernameFlag, "NAV_OSS_ADMIN_USERNAME", configuration.Auth.Username, "")

	password := firstNonEmpty(os.Getenv("NAV_OSS_ADMIN_PASSWORD"), configuration.Auth.Password)
	passwordHash := firstNonEmpty(os.Getenv("NAV_OSS_ADMIN_PASSWORD_HASH"), configuration.Auth.PasswordHash)
	passwordFile := firstNonEmpty(os.Getenv("NAV_OSS_ADMIN_PASSWORD_FILE"), configuration.Auth.PasswordFile)
	if password == "" && passwordFile != "" {
		resolvedPasswordFile := passwordFile
		if configPath != "" && !filepath.IsAbs(resolvedPasswordFile) {
			resolvedPasswordFile = filepath.Join(filepath.Dir(configPath), resolvedPasswordFile)
		}
		content, readErr := os.ReadFile(resolvedPasswordFile)
		if readErr != nil {
			return options{}, fmt.Errorf("read admin password file: %w", readErr)
		}
		password = strings.TrimRight(string(content), "\r\n")
	}

	sessionMinutes := configuration.Auth.SessionTTLMinutes
	if value := os.Getenv("NAV_OSS_SESSION_TTL_MINUTES"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			return options{}, fmt.Errorf("parse NAV_OSS_SESSION_TTL_MINUTES: %w", parseErr)
		}
		sessionMinutes = parsed
	}
	if visited["session-ttl-minutes"] {
		sessionMinutes = *sessionFlag
	}
	if sessionMinutes <= 0 {
		sessionMinutes = 24 * 60
	}

	return options{
		APIAddr: apiAddress, ConsoleAddr: consoleAddress, StateFile: stateFile, RuntimeDir: runtimeDir,
		AdminUsername: username, AdminPassword: password, AdminPasswordHash: passwordHash,
		SessionTTL: time.Duration(sessionMinutes) * time.Minute,
	}, nil
}

func loadFileConfig(path string) (fileConfig, error) {
	if strings.TrimSpace(path) == "" {
		return fileConfig{}, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, err
	}
	var configuration fileConfig
	if err = json.Unmarshal(content, &configuration); err != nil {
		return fileConfig{}, err
	}
	return configuration, nil
}

func resolveOption(flagSet bool, flagValue, environmentName, fileValue, fallback string) string {
	if flagSet {
		return flagValue
	}
	return firstNonEmpty(os.Getenv(environmentName), fileValue, fallback)
}

func valueIf(condition bool, value string) string {
	if condition {
		return value
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func displayURL(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "http://" + address
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
