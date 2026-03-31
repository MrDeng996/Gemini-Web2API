package adapter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"gemini-web2api/internal/balancer"
	"gemini-web2api/internal/claude"
	"gemini-web2api/internal/config"
	"gemini-web2api/internal/gemini"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func ClaudeMessagesHandler(pool *balancer.AccountPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		client, accountID := pool.Next()
		if client == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "overloaded_error",
					"message": "No available accounts",
				},
			})
			return
		}

		c.Set("account_id", accountID)

		var req claude.ClaudeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": fmt.Sprintf("Invalid request body: %v", err),
				},
			})
			return
		}

		if len(req.Messages) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": "messages array is required and cannot be empty",
				},
			})
			return
		}

		log.Printf("[Claude] Request | Model: %s | Stream: %v | Messages: %d | Tools: %d",
			req.Model, req.Stream, len(req.Messages), len(req.Tools))

		mappedModel := config.MapModel(req.Model)
		if mappedModel != req.Model {
			log.Printf("[Claude] Model mapped: %s -> %s", req.Model, mappedModel)
		}

		prompt, files := buildClaudePrompt(&req, client)

		gemini.RandomDelay()

		respBody, err := client.StreamGenerateContent(prompt, mappedModel, files, nil)
		if err != nil {
			log.Printf("[Claude] Gemini request failed: %v", err)
			pool.MarkFailed(accountID)
			c.JSON(http.StatusInternalServerError, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "api_error",
					"message": fmt.Sprintf("Failed to communicate with Gemini: %v", err),
				},
			})
			return
		}
		pool.MarkSuccess(accountID)
		defer respBody.Close()

		if req.Stream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("Transfer-Encoding", "chunked")

			// 启动 SSE 保活，防止反向代理因长时间无数据而断开连接
			kaStop := make(chan struct{})
			go StartSSEKeepalive(c.Writer, kaStop)

			// 流式：累积全文以供截断检测
			var streamedText strings.Builder
			c.Stream(func(w io.Writer) bool {
				processor := claude.NewStreamProcessor(req.Model, w)
				processor.SetTextCallback(func(t string) { streamedText.WriteString(t) })
				processor.ProcessGeminiStream(respBody)
				return false
			})

			// 截断续写：需要在 finalize() 已发送 message_stop 之后
			// 重新开启一个 content_block，发送续写内容，再关闭。
			var contBlockOpened bool
			AutoContinueStream(client, mappedModel, prompt, streamedText.String(),
				func(text string) {
					if !contBlockOpened {
						// 重新开启 content block（续写专用，index=1 避免与原 block 冲突）
						start := map[string]interface{}{
							"type":         "content_block_start",
							"index":        1,
							"content_block": map[string]interface{}{"type": "text", "text": ""},
						}
						b, _ := json.Marshal(start)
						fmt.Fprintf(c.Writer, "event: content_block_start\ndata: %s\n\n", b)
						c.Writer.(http.Flusher).Flush()
						contBlockOpened = true
					}
					delta := map[string]interface{}{
						"type":  "content_block_delta",
						"index": 1,
						"delta": map[string]interface{}{"type": "text_delta", "text": text},
					}
					b, _ := json.Marshal(delta)
					fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", b)
					c.Writer.(http.Flusher).Flush()
				})
			if contBlockOpened {
				// 关闭续写 content block
				stop := map[string]interface{}{"type": "content_block_stop", "index": 1}
				b, _ := json.Marshal(stop)
				fmt.Fprintf(c.Writer, "event: content_block_stop\ndata: %s\n\n", b)
				// 补发 message_stop（原 finalize() 已发过，但客户端需要知道真正结束）
				fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				c.Writer.(http.Flusher).Flush()
			}

			close(kaStop)
		} else {
			var fullText string
			var fullThinking string

			parseGeminiResponse(respBody, func(text, thought string) {
				fullText += text
				fullThinking += thought
			})

			// 截断续写：非流式模式下阻塞重试
			fullText = AutoContinueNonStream(client, mappedModel, prompt, fullText)

			var contentBlocks []claude.ContentBlock

			if fullThinking != "" {
				contentBlocks = append(contentBlocks, claude.ContentBlock{
					Type:     "thinking",
					Thinking: fullThinking,
				})
			}

			if fullText != "" {
				contentBlocks = append(contentBlocks, claude.ContentBlock{
					Type: "text",
					Text: fullText,
				})
			}

			if len(contentBlocks) == 0 {
				contentBlocks = append(contentBlocks, claude.ContentBlock{
					Type: "text",
					Text: "",
				})
			}

			response := claude.ClaudeResponse{
				ID:         fmt.Sprintf("msg_%d", time.Now().UnixNano()),
				Type:       "message",
				Role:       "assistant",
				Model:      req.Model,
				Content:    contentBlocks,
				StopReason: "end_turn",
				Usage: claude.Usage{
					InputTokens:  0,
					OutputTokens: 0,
				},
			}

			c.JSON(http.StatusOK, response)
		}
	}
}

func ClaudeListModelsHandler(c *gin.Context) {
	models := []map[string]interface{}{
		{
			"id":           "gemini-2.5-flash",
			"display_name": "Gemini 2.5 Flash",
			"created_at":   time.Now().Format(time.RFC3339),
		},
		{
			"id":           "gemini-3.1-pro-preview",
			"display_name": "Gemini 3.1 Pro Preview",
			"created_at":   time.Now().Format(time.RFC3339),
		},
		{
			"id":           "gemini-3-flash-preview",
			"display_name": "Gemini 3 Flash Preview",
			"created_at":   time.Now().Format(time.RFC3339),
		},
		{
			"id":           "gemini-3-flash-preview-no-thinking",
			"display_name": "Gemini 3 Flash Preview (No Thinking)",
			"created_at":   time.Now().Format(time.RFC3339),
		},
	}

	c.JSON(http.StatusOK, gin.H{
		"data":     models,
		"has_more": false,
		"object":   "list",
	})
}

func ClaudeCountTokensHandler(pool *balancer.AccountPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req claude.ClaudeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": fmt.Sprintf("Invalid request body: %v", err),
				},
			})
			return
		}

		tokenCount := 0
		for _, msg := range req.Messages {
			blocks, strContent, _ := claude.ParseMessageContent(msg.Content)
			if strContent != "" {
				tokenCount += len(strContent) / 4
			} else {
				for _, block := range blocks {
					if block.Type == "text" {
						tokenCount += len(block.Text) / 4
					}
				}
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"input_tokens": tokenCount,
		})
	}
}

func buildClaudePrompt(req *claude.ClaudeRequest, client *gemini.Client) (string, []gemini.FileData) {
	var builder strings.Builder
	var files []gemini.FileData

	if req.System != nil {
		if sysPrompt, _ := claude.ParseSystemPrompt(req.System); sysPrompt != "" {
			builder.WriteString("**System**: ")
			builder.WriteString(sysPrompt)
			builder.WriteString("\n\n")
		}
	}

	for _, msg := range req.Messages {
		role := "User"
		if msg.Role == "assistant" {
			role = "Model"
		} else if msg.Role == "system" {
			role = "System"
		}

		builder.WriteString(fmt.Sprintf("**%s**: ", role))

		blocks, strContent, err := claude.ParseMessageContent(msg.Content)
		if err != nil {
			continue
		}

		if strContent != "" {
			builder.WriteString(strContent)
		} else {
			for _, block := range blocks {
				switch block.Type {
				case "text":
					builder.WriteString(block.Text)
				case "thinking":
					builder.WriteString(fmt.Sprintf("<thinking>%s</thinking>", block.Thinking))
				case "tool_use":
					argsJSON, _ := json.Marshal(block.Input)
					builder.WriteString(fmt.Sprintf("<tool_use id=\"%s\" name=\"%s\">%s</tool_use>",
						block.ID, block.Name, string(argsJSON)))
				case "tool_result":
					var contentStr string
					if block.Content != nil {
						json.Unmarshal(block.Content, &contentStr)
					}
					builder.WriteString(fmt.Sprintf("<tool_result id=\"%s\">%s</tool_result>",
						block.ToolUseID, contentStr))
				case "image":
					if block.Source != nil && block.Source.Type == "base64" {
						data, err := base64.StdEncoding.DecodeString(block.Source.Data)
						if err == nil {
							fname := fmt.Sprintf("image_%d.png", time.Now().UnixNano())
							fid, err := client.UploadFile(data, fname)
							if err == nil {
								files = append(files, gemini.FileData{
									URL:      fid,
									FileName: fname,
								})
								builder.WriteString("[Image]")
							}
						}
					}
				}
			}
		}

		builder.WriteString("\n\n")
	}

	finalPrompt := builder.String()
	if finalPrompt == "" {
		finalPrompt = "Hello"
	}

	return finalPrompt, files
}
