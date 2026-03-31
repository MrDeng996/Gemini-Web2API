package adapter

import (
	"sync/atomic"

	"gemini-web2api/internal/balancer"
	"github.com/gin-gonic/gin"
)

var metricsState = struct {
	openAIRequests      atomic.Uint64
	claudeRequests      atomic.Uint64
	geminiRequests      atomic.Uint64
	toolCallsParsed     atomic.Uint64
	toolParseFailures   atomic.Uint64
	rateLimitRejections atomic.Uint64
}{}

func recordOpenAIRequest() {
	metricsState.openAIRequests.Add(1)
}

func recordClaudeRequest() {
	metricsState.claudeRequests.Add(1)
}

func recordGeminiRequest() {
	metricsState.geminiRequests.Add(1)
}

func recordToolCallsParsed(count int) {
	if count > 0 {
		metricsState.toolCallsParsed.Add(uint64(count))
	}
}

func recordToolParseFailure() {
	metricsState.toolParseFailures.Add(1)
}

func recordRateLimitRejection() {
	metricsState.rateLimitRejections.Add(1)
}

func MetricsHandler(pool *balancer.AccountPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, gin.H{
			"requests": gin.H{
				"openai": metricsState.openAIRequests.Load(),
				"claude": metricsState.claudeRequests.Load(),
				"gemini": metricsState.geminiRequests.Load(),
			},
			"tool_bridge": gin.H{
				"parsed_calls":   metricsState.toolCallsParsed.Load(),
				"parse_failures": metricsState.toolParseFailures.Load(),
			},
			"rate_limit": gin.H{
				"rejections": metricsState.rateLimitRejections.Load(),
			},
			"accounts": gin.H{
				"available": pool.Size(),
			},
		})
	}
}
