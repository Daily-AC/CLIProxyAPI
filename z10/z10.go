package z10

import (
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// ServerOptions wires friend keys into the API server: it registers the access provider
// and the usage plugin, and returns the middleware and admin-route options. Call it once,
// after the built-in access providers are registered, before the server is built.
func ServerOptions(configFilePath string) []api.ServerOption {
	if strings.TrimSpace(configFilePath) == "" {
		return nil
	}
	rt := NewRuntime(Options{ConfigPath: configFilePath})
	rt.flushOnSignal = true
	rt.Register()
	return rt.ServerOptions()
}

// Register installs the access provider and the usage plugin in the global registries.
func (rt *Runtime) Register() {
	sdkaccess.RegisterProvider(AccessProviderType, &accessProvider{rt: rt})
	coreusage.RegisterNamedPlugin(AccessProviderType, &usagePlugin{rt: rt})
}

// ServerOptions returns the middleware and router options for this runtime.
func (rt *Runtime) ServerOptions() []api.ServerOption {
	return []api.ServerOption{
		api.WithMiddleware(rt.Middleware()),
		api.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
			// Background work starts only when a server is actually built, not in
			// one-shot command modes such as --claude-login.
			rt.Start()
			if rt.flushOnSignal {
				rt.startSignalFlush()
			}
			rt.registerAdminRoutes(engine)
		}),
	}
}

// startSignalFlush persists usage when shutdown begins and switches to synchronous
// writes, so requests finishing during graceful shutdown are not lost.
func (rt *Runtime) startSignalFlush() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-signals:
			rt.usage.FlushOnEveryChange()
			rt.usage.flushFromTimer()
		case <-rt.done:
			signal.Stop(signals)
		}
	}()
}
