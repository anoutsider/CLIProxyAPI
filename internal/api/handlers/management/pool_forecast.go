package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// GetPoolForecast returns the Claude pool forecast: each account's weekly
// windows, the pool's burn and projection, and alerts. It holds no credential.
func (h *Handler) GetPoolForecast(c *gin.Context) {
	now := time.Now()
	history, errLoad := coreauth.DefaultPoolHistory().Load(c.Request.Context(), now)
	if errLoad != nil {
		log.Warnf("pool forecast: %v", errLoad)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "pool history unavailable"})
		return
	}
	input := coreauth.PoolForecastInput{Now: now, History: history}
	if cfg := h.configSnapshot(); cfg != nil {
		input.Threshold = cfg.Routing.SoonestResetThreshold
		input.DrainWindow = coreauth.ParseSoonestResetDrainWindow(cfg.Routing.SoonestResetDrainWindow)
	}
	if h.authManager != nil {
		input.Accounts = coreauth.ClaudePoolAccounts(h.authManager.List(), now)
	}
	c.JSON(http.StatusOK, coreauth.BuildPoolForecast(input))
}
