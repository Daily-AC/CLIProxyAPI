package z10

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// friendView is the public description of a friend key. It never includes the key.
type friendView struct {
	Name     string     `json:"name"`
	Models   []string   `json:"models"`
	Channels []string   `json:"channels"`
	Expires  string     `json:"expires,omitempty"`
	Enabled  bool       `json:"enabled"`
	Active   bool       `json:"active"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

// registerAdminRoutes adds GET /z10/usage and GET /z10/friends, guarded by the
// management key exactly like the management API (same handler and config fields).
func (rt *Runtime) registerAdminRoutes(engine *gin.Engine) {
	group := engine.Group("/z10", rt.managementAvailable, rt.mgmt.Middleware())
	group.GET("/usage", rt.handleUsage)
	group.GET("/friends", rt.handleFriends)
}

// managementAvailable mirrors the server's availability gate: the management API is not
// served when the node is controlled by CLIProxyAPIHome.
func (rt *Runtime) managementAvailable(c *gin.Context) {
	if owner := rt.state.Load().owner; owner != nil && owner.Home.Enabled {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Next()
}

func (rt *Runtime) handleUsage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, rt.usage.Report())
}

func (rt *Runtime) handleFriends(c *gin.Context) {
	now := rt.now()
	report := rt.usage.Report()
	friends := rt.Friends().Friends()
	views := make([]friendView, 0, len(friends))
	for _, friend := range friends {
		view := friendView{
			Name:     friend.Name,
			Models:   append([]string{}, friend.Models...),
			Channels: append([]string{}, friend.Channels...),
			Expires:  friend.ExpiresRaw,
			Enabled:  friend.Enabled,
			Active:   friend.inactiveReason(now) == "",
		}
		if usage := report.Friends[friend.Name]; usage != nil {
			view.LastUsed = usage.LastUsed
		}
		views = append(views, view)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"friends": views})
}
