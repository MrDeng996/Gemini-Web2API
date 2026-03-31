package adapter

import (
	"log"
	"strings"

	"gemini-web2api/internal/gemini"
)

const (
	// continuationContextLen 续写时取末尾多少字符作为上下文
	continuationContextLen = 2000
	// maxContinuations 最多自动续写次数，防止无限循环
	maxContinuations = 3
	// continuationPrompt 续写指令（追加到 prompt 末尾）
	continuationPrompt = "\n\n[系统提示：你的上一次回复似乎被截断了，请从断点处继续完成回复，不要重复已经说过的内容]"
	// overlapSearchLen 在续写内容头部搜索重叠时的最大搜索窗口（字符数）
	overlapSearchLen = 200
)

// IsTruncated 检测响应文本是否存在截断迹象。
// 截断特征（参考 cursor2api 实现）：
//  1. 未闭合的代码块  ```（奇数个）
//  2. 未闭合的 XML/HTML 尖括号（简单启发式）
//  3. 末尾以开放符号结束：`[` `(` `{` `,` `，`
//  4. 末尾以省略号结束
func IsTruncated(text string) bool {
	if text == "" {
		return false
	}

	// 1. 检测未闭合代码块（奇数个 ``` 标记）
	codeBlockCount := strings.Count(text, "```")
	if codeBlockCount%2 != 0 {
		log.Printf("[Truncation] 检测到未闭合代码块 (count=%d)", codeBlockCount)
		return true
	}

	// 2. 检测末尾开放符号
	trimmed := strings.TrimRight(text, " \t\n\r")
	if trimmed == "" {
		return false
	}
	runes := []rune(trimmed)
	lastRune := runes[len(runes)-1]
	openEnders := map[rune]bool{
		'[': true, '(': true, '{': true,
		',': true, '，': true,
	}
	if openEnders[lastRune] {
		log.Printf("[Truncation] 末尾开放符号: %q", string(lastRune))
		return true
	}

	// 3. 检测省略号结尾
	if strings.HasSuffix(trimmed, "...") || strings.HasSuffix(trimmed, "……") ||
		strings.HasSuffix(trimmed, "…") {
		log.Printf("[Truncation] 检测到省略号结尾")
		return true
	}

	// 4. 检测未闭合 XML/HTML 标签（尖括号数量不匹配，允许 1 个误差）
	openAngle := strings.Count(text, "<")
	closeAngle := strings.Count(text, ">")
	if openAngle > closeAngle+1 {
		log.Printf("[Truncation] 未闭合 XML/HTML 标签 (open=%d close=%d)", openAngle, closeAngle)
		return true
	}

	return false
}

// DeduplicateContinuation 去除续写内容开头与已有内容末尾的重叠部分。
// 算法：在 continuation 头部的 overlapSearchLen 字符窗口内，
// 从最长到最短搜索与 existing 末尾匹配的重叠，找到则截掉该前缀。
// 最小有效重叠长度为 10 个字符，避免误判。
func DeduplicateContinuation(existing, continuation string) string {
	if continuation == "" {
		return ""
	}

	existRunes := []rune(existing)
	contRunes := []rune(continuation)

	// 取 existing 末尾 overlapSearchLen 个字符作为参考窗口
	existTail := existRunes
	if len(existRunes) > overlapSearchLen {
		existTail = existRunes[len(existRunes)-overlapSearchLen:]
	}

	// 取 continuation 头部 overlapSearchLen 个字符作为搜索窗口
	contHead := contRunes
	if len(contRunes) > overlapSearchLen {
		contHead = contRunes[:overlapSearchLen]
	}

	// 从最长到最短搜索重叠（至少 10 个字符）
	maxOverlap := len(existTail)
	if len(contHead) < maxOverlap {
		maxOverlap = len(contHead)
	}
	for overlapLen := maxOverlap; overlapLen >= 10; overlapLen-- {
		existSuffix := string(existTail[len(existTail)-overlapLen:])
		contPrefix := string(contHead[:overlapLen])
		if existSuffix == contPrefix {
			log.Printf("[Truncation] 去重重叠: %d 个字符", overlapLen)
			return string(contRunes[overlapLen:])
		}
	}
	return continuation
}

// ContinuationResult 单次续写结果
type ContinuationResult struct {
	Text string // 本次续写获得的新文本（已去重）
	Done bool   // true = 续写后不再截断，可以停止
}

// fetchContinuation 使用相同客户端向 Gemini 发起一次续写请求。
// originalPrompt: 原始对话 prompt
// accumulatedText: 截至目前已收到的全部文本
// onChunk: 流式片段回调（非流式传 nil）
func fetchContinuation(
	client *gemini.Client,
	model string,
	originalPrompt string,
	accumulatedText string,
	onChunk func(text string),
) ContinuationResult {
	// 取末尾 context 构造续写 prompt
	existRunes := []rune(accumulatedText)
	tailStart := 0
	if len(existRunes) > continuationContextLen {
		tailStart = len(existRunes) - continuationContextLen
	}
	tailContext := string(existRunes[tailStart:])

	continuationFullPrompt := originalPrompt +
		"\n\n[模型之前的回复末尾]:\n" + tailContext +
		continuationPrompt

	respBody, err := client.StreamGenerateContent(continuationFullPrompt, model, nil, nil)
	if err != nil {
		log.Printf("[Truncation] 续写请求失败: %v", err)
		return ContinuationResult{Text: "", Done: true}
	}
	defer respBody.Close()

	var sb strings.Builder
	parseGeminiResponse(respBody, func(text, _ string) {
		if text == "" {
			return
		}
		sb.WriteString(text)
		if onChunk != nil {
			onChunk(text)
		}
	})

	newRaw := sb.String()
	if newRaw == "" {
		return ContinuationResult{Text: "", Done: true}
	}

	// 去除与原文末尾的重叠
	deduped := DeduplicateContinuation(accumulatedText, newRaw)
	merged := accumulatedText + deduped

	return ContinuationResult{
		Text: deduped,
		Done: !IsTruncated(merged),
	}
}

// AutoContinueNonStream 非流式截断续写：阻塞式循环，最多续写 maxContinuations 次。
// 返回完整文本（原始 + 所有续写片段拼接）。
func AutoContinueNonStream(
	client *gemini.Client,
	model string,
	originalPrompt string,
	initialText string,
) string {
	if !IsTruncated(initialText) {
		return initialText
	}

	full := initialText
	for i := 0; i < maxContinuations; i++ {
		log.Printf("[Truncation] 非流式续写第 %d 次", i+1)
		result := fetchContinuation(client, model, originalPrompt, full, nil)
		if result.Text == "" {
			break
		}
		full += result.Text
		if result.Done {
			break
		}
	}
	return full
}

// AutoContinueStream 流式截断续写：每次续写的片段通过 onChunk 实时推送给客户端。
// initialText 是调用方已经推送完毕的完整第一次响应文本。
// 返回完整的最终拼接文本（用于日志）。
func AutoContinueStream(
	client *gemini.Client,
	model string,
	originalPrompt string,
	initialText string,
	onChunk func(text string),
) string {
	if !IsTruncated(initialText) {
		return initialText
	}

	full := initialText
	for i := 0; i < maxContinuations; i++ {
		log.Printf("[Truncation] 流式续写第 %d 次", i+1)

		// fetchContinuation 内部已做 DeduplicateContinuation，result.Text 是去重后文本
		result := fetchContinuation(client, model, originalPrompt, full, nil)

		if result.Text == "" {
			break
		}

		// 推送去重后的片段
		if onChunk != nil {
			onChunk(result.Text)
		}
		full += result.Text

		if result.Done {
			break
		}
	}
	return full
}
