package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type OpenAIToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type OpenAITool struct {
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

type ParsedToolCall struct {
	Name      string
	Arguments map[string]interface{}
}

var (
	toolBlockOpenPattern = regexp.MustCompile("```json(?:\\s+action)?")
	trailingCommaPattern = regexp.MustCompile(`,\s*([}\]])`)
	toolNamePattern      = regexp.MustCompile(`(?s)[\"'](?:tool|name)[\"']\s*:\s*[\"']([^\"']+)[\"']`)
	paramObjectPattern   = regexp.MustCompile(`(?s)[\"'](?:parameters|arguments|input)[\"']\s*:\s*(\{[\s\S]*)`)
	smallFieldRegex      = regexp.MustCompile(`"(file_path|path|file|old_string|old_str|insert_line|mode|encoding|description|language|name)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

var smartDoubleQuotes = map[rune]bool{
	'«': true, '“': true, '”': true, '❞': true,
	'‟': true, '„': true, '❝': true, '»': true,
}

var smartSingleQuotes = map[rune]bool{
	'‘': true, '’': true, '‚': true, '‛': true,
}

func buildToolInstructions(tools []OpenAITool, toolChoice interface{}) string {
	if len(tools) == 0 {
		return ""
	}

	schemaMode := getToolSchemaMode()

	var sb strings.Builder
	sb.WriteString("You have access to the following tools. When you need to call a tool, you MUST output it in exactly this format:\n\n")
	sb.WriteString("```json action\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"tool\": \"TOOL_NAME\",\n")
	sb.WriteString("  \"parameters\": {\n")
	sb.WriteString("    \"param\": \"value\"\n")
	sb.WriteString("  }\n")
	sb.WriteString("}\n")
	sb.WriteString("```\n\n")
	sb.WriteString("Available tools:\n")

	for _, tool := range tools {
		if strings.TrimSpace(tool.Function.Name) == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("- %s", tool.Function.Name))
		if strings.TrimSpace(tool.Function.Description) != "" {
			sb.WriteString(fmt.Sprintf(": %s", tool.Function.Description))
		}
		if len(tool.Function.Parameters) > 0 {
			sb.WriteString(fmt.Sprintf("\n  Parameters: %s", formatToolSchema(tool.Function.Parameters, schemaMode)))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\nRules:\n")
	sb.WriteString("- Use the json action block only when invoking a tool.\n")
	sb.WriteString("- You may include normal text before tool calls if needed.\n")
	sb.WriteString("- For dependent actions, wait for tool results before calling another tool.\n")
	appendToolChoiceInstruction(&sb, toolChoice)

	return sb.String()
}

func appendToolChoiceInstruction(sb *strings.Builder, toolChoice interface{}) {
	if toolChoice == nil {
		return
	}

	switch v := toolChoice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "required":
			sb.WriteString("- You must call at least one tool in your response.\n")
		case "none":
			sb.WriteString("- Do not call any tools in this response.\n")
		}
	case map[string]interface{}:
		choiceType, _ := v["type"].(string)
		if strings.EqualFold(choiceType, "function") {
			if fnMap, ok := v["function"].(map[string]interface{}); ok {
				if name, ok := fnMap["name"].(string); ok && strings.TrimSpace(name) != "" {
					sb.WriteString(fmt.Sprintf("- You must call the tool named %q in your response.\n", name))
				}
			}
		}
	}
}

func injectToolPrompt(prompt string, tools []OpenAITool, toolChoice interface{}) string {
	instructions := buildToolInstructions(tools, toolChoice)
	if instructions == "" {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return instructions
	}
	return instructions + "\n\n---\n\n" + prompt
}

func hasToolCalls(text string) bool {
	return strings.Contains(text, "```json")
}

func parseToolCalls(responseText string) ([]ParsedToolCall, string) {
	var toolCalls []ParsedToolCall
	var blocksToRemove [][2]int

	matches := toolBlockOpenPattern.FindAllStringIndex(responseText, -1)
	for _, match := range matches {
		blockStart := match[0]
		contentStart := match[1]

		closingPos := findToolBlockClosing(responseText, contentStart)
		var jsonContent string
		var blockEnd int

		if closingPos >= 0 {
			jsonContent = strings.TrimSpace(responseText[contentStart:closingPos])
			blockEnd = closingPos + 3
		} else {
			jsonContent = strings.TrimSpace(responseText[contentStart:])
			blockEnd = len(responseText)
		}

		if len(jsonContent) <= 2 {
			continue
		}

		parsed, err := tolerantParseToolCall(jsonContent)
		if err != nil || strings.TrimSpace(parsed.Name) == "" {
			log.Printf("[ToolBridge] failed to parse tool block: %v", err)
			continue
		}

		parsed.Arguments = fixToolCallArguments(parsed.Name, parsed.Arguments)
		log.Printf("[ToolBridge] parsed tool call: name=%s args=%d", parsed.Name, len(parsed.Arguments))
		toolCalls = append(toolCalls, parsed)
		blocksToRemove = append(blocksToRemove, [2]int{blockStart, blockEnd})
	}

	cleanText := responseText
	for i := len(blocksToRemove) - 1; i >= 0; i-- {
		block := blocksToRemove[i]
		cleanText = cleanText[:block[0]] + cleanText[block[1]:]
	}

	return toolCalls, strings.TrimSpace(cleanText)
}

func findToolBlockClosing(text string, contentStart int) int {
	inString := false
	for pos := contentStart; pos < len(text)-2; pos++ {
		if text[pos] == '"' {
			backslashCount := 0
			for j := pos - 1; j >= contentStart && text[j] == '\\'; j-- {
				backslashCount++
			}
			if backslashCount%2 == 0 {
				inString = !inString
			}
			continue
		}
		if !inString && text[pos:pos+3] == "```" {
			return pos
		}
	}
	return -1
}

func tolerantParseToolCall(jsonStr string) (ParsedToolCall, error) {
	jsonStr = replaceSmartQuotes(jsonStr)

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err == nil {
		return parsedToolCallFromMap(raw), nil
	}

	fixed := fixBrokenJSONString(jsonStr)
	if err := json.Unmarshal([]byte(fixed), &raw); err == nil {
		return parsedToolCallFromMap(raw), nil
	}

	lastBrace := strings.LastIndex(fixed, "}")
	if lastBrace > 0 {
		if err := json.Unmarshal([]byte(fixed[:lastBrace+1]), &raw); err == nil {
			return parsedToolCallFromMap(raw), nil
		}
	}

	if parsed, ok := extractToolAndParamsByRegex(jsonStr); ok {
		return parsed, nil
	}

	if parsed, ok := extractToolAndParamsGreedy(jsonStr); ok {
		return parsed, nil
	}

	return ParsedToolCall{}, fmt.Errorf("tool call parse failed")
}

func fixBrokenJSONString(s string) string {
	s = replaceSmartQuotes(s)

	var sb strings.Builder
	inString := false
	stack := make([]rune, 0, 8)

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			backslashCount := 0
			for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
				backslashCount++
			}
			if backslashCount%2 == 0 {
				inString = !inString
			}
			sb.WriteByte(ch)
			continue
		}

		if inString {
			switch ch {
			case '\n':
				sb.WriteString(`\n`)
			case '\r':
				sb.WriteString(`\r`)
			case '\t':
				sb.WriteString(`\t`)
			default:
				sb.WriteByte(ch)
			}
			continue
		}

		switch ch {
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		sb.WriteByte(ch)
	}

	if inString {
		sb.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		sb.WriteRune(stack[i])
	}

	return trailingCommaPattern.ReplaceAllString(sb.String(), "$1")
}

func extractToolAndParamsByRegex(jsonStr string) (ParsedToolCall, bool) {
	nameMatch := toolNamePattern.FindStringSubmatch(jsonStr)
	if len(nameMatch) < 2 {
		return ParsedToolCall{}, false
	}

	args := map[string]interface{}{}
	paramMatch := paramObjectPattern.FindStringSubmatch(jsonStr)
	if len(paramMatch) >= 2 {
		paramsStr := paramMatch[1]
		if rawParams, ok := extractJSONObjectPrefix(paramsStr); ok {
			if err := json.Unmarshal([]byte(fixBrokenJSONString(rawParams)), &args); err != nil {
				for _, match := range smallFieldRegex.FindAllStringSubmatch(rawParams, -1) {
					if len(match) >= 3 {
						args[match[1]] = unescapeBasicString(match[2])
					}
				}
			}
		}
	}

	return ParsedToolCall{Name: nameMatch[1], Arguments: args}, true
}

func extractToolAndParamsGreedy(jsonStr string) (ParsedToolCall, bool) {
	nameMatch := toolNamePattern.FindStringSubmatch(jsonStr)
	if len(nameMatch) < 2 {
		return ParsedToolCall{}, false
	}

	params := map[string]interface{}{}
	for _, match := range smallFieldRegex.FindAllStringSubmatch(jsonStr, -1) {
		if len(match) >= 3 {
			params[match[1]] = unescapeBasicString(match[2])
		}
	}

	bigValueFields := []string{"content", "command", "text", "new_string", "new_str", "file_text", "code"}
	for _, field := range bigValueFields {
		if value, ok := extractGreedyStringField(jsonStr, field); ok {
			params[field] = value
		}
	}

	if len(params) == 0 {
		return ParsedToolCall{}, false
	}
	return ParsedToolCall{Name: nameMatch[1], Arguments: params}, true
}

func extractJSONObjectPrefix(s string) (string, bool) {
	depth := 0
	inString := false
	start := -1

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			backslashCount := 0
			for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
				backslashCount++
			}
			if backslashCount%2 == 0 {
				inString = !inString
			}
		}
		if inString {
			continue
		}
		if c == '{' {
			if start == -1 {
				start = i
			}
			depth++
		}
		if c == '}' {
			depth--
			if depth == 0 && start >= 0 {
				return s[start : i+1], true
			}
		}
	}

	if start >= 0 {
		return s[start:], true
	}
	return "", false
}

func extractGreedyStringField(jsonStr string, field string) (string, bool) {
	fieldStart := strings.Index(jsonStr, fmt.Sprintf("\"%s\"", field))
	if fieldStart == -1 {
		return "", false
	}

	colonPos := strings.Index(jsonStr[fieldStart+len(field)+2:], ":")
	if colonPos == -1 {
		return "", false
	}
	colonPos += fieldStart + len(field) + 2

	valueStart := strings.Index(jsonStr[colonPos:], "\"")
	if valueStart == -1 {
		return "", false
	}
	valueStart += colonPos

	valueEnd := len(jsonStr) - 1
	for valueEnd > valueStart && strings.ContainsRune("}]\n\r\t ,", rune(jsonStr[valueEnd])) {
		valueEnd--
	}
	if valueEnd <= valueStart || jsonStr[valueEnd] != '"' {
		return "", false
	}

	rawValue := jsonStr[valueStart+1 : valueEnd]
	if decoded, err := decodeJSONStringValue(rawValue); err == nil {
		return decoded, true
	}
	return unescapeBasicString(rawValue), true
}

func decodeJSONStringValue(raw string) (string, error) {
	var out string
	err := json.Unmarshal([]byte("\""+raw+"\""), &out)
	return out, err
}

func unescapeBasicString(s string) string {
	s = replaceSmartQuotes(s)
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\t`, "\t")
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\\`, `\`)
	s = strings.ReplaceAll(s, `\"`, `"`)
	return s
}

func parsedToolCallFromMap(raw map[string]interface{}) ParsedToolCall {
	name, _ := raw["tool"].(string)
	if name == "" {
		name, _ = raw["name"].(string)
	}
	args := map[string]interface{}{}
	for _, key := range []string{"parameters", "arguments", "input"} {
		if val, ok := raw[key].(map[string]interface{}); ok {
			args = val
			break
		}
	}
	return ParsedToolCall{Name: name, Arguments: args}
}

func replaceSmartQuotes(text string) string {
	chars := []rune(text)
	for i, ch := range chars {
		if smartDoubleQuotes[ch] {
			chars[i] = '"'
			continue
		}
		if smartSingleQuotes[ch] {
			chars[i] = '\''
		}
	}
	return string(chars)
}

func fixToolCallArguments(toolName string, args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return map[string]interface{}{}
	}

	for key, value := range args {
		if s, ok := value.(string); ok {
			args[key] = replaceSmartQuotes(s)
		}
	}

	return repairExactMatchToolArguments(toolName, args)
}

func repairExactMatchToolArguments(toolName string, args map[string]interface{}) map[string]interface{} {
	if !toolArgRepairEnabled() {
		return args
	}

	lowerName := strings.ToLower(strings.TrimSpace(toolName))
	if !strings.Contains(lowerName, "str_replace") &&
		!strings.Contains(lowerName, "search_replace") &&
		!strings.Contains(lowerName, "strreplace") {
		return args
	}

	oldString, _ := stringFromAny(firstNonNil(args["old_string"], args["old_str"]))
	if strings.TrimSpace(oldString) == "" {
		return args
	}

	filePath, _ := stringFromAny(firstNonNil(args["path"], args["file_path"]))
	if strings.TrimSpace(filePath) == "" {
		return args
	}

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return args
	}
	if fileInfo.Size() > getToolArgRepairMaxFileSize() {
		log.Printf("[ToolBridge] skip exact-match repair for %s: file too large (%d bytes)", filePath, fileInfo.Size())
		return args
	}

	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return args
	}
	content := string(contentBytes)
	if strings.Contains(content, oldString) {
		return args
	}

	pattern := buildFuzzyPattern(oldString)
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return args
	}
	matches := regex.FindAllString(content, -1)
	if len(matches) != 1 {
		return args
	}

	matchedText := matches[0]
	if _, ok := args["old_string"]; ok {
		args["old_string"] = matchedText
	} else if _, ok := args["old_str"]; ok {
		args["old_str"] = matchedText
	}

	if newString, ok := stringFromAny(firstNonNil(args["new_string"], args["new_str"])); ok {
		fixed := replaceSmartQuotes(newString)
		if _, exists := args["new_string"]; exists {
			args["new_string"] = fixed
		} else if _, exists := args["new_str"]; exists {
			args["new_str"] = fixed
		}
	}

	return args
}

func getToolSchemaMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("TOOL_SCHEMA_MODE")))
	switch mode {
	case "compact", "full", "names_only":
		return mode
	default:
		return "compact"
	}
}

func formatToolSchema(schema map[string]interface{}, mode string) string {
	switch mode {
	case "names_only":
		if props, ok := schema["properties"].(map[string]interface{}); ok {
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			return strings.Join(names, ", ")
		}
		return "{}"
	case "full":
		bytes, _ := json.Marshal(schema)
		return string(bytes)
	default:
		return compactSchema(schema)
	}
}

func compactSchema(schema map[string]interface{}) string {
	propsRaw, ok := schema["properties"].(map[string]interface{})
	if !ok || len(propsRaw) == 0 {
		return "{}"
	}

	requiredSet := map[string]bool{}
	if required, ok := schema["required"].([]interface{}); ok {
		for _, item := range required {
			if s, ok := item.(string); ok {
				requiredSet[s] = true
			}
		}
	}

	parts := make([]string, 0, len(propsRaw))
	for name, rawProp := range propsRaw {
		prop, _ := rawProp.(map[string]interface{})
		typeName, _ := prop["type"].(string)
		if typeName == "" {
			typeName = "any"
		}
		if enumVals, ok := prop["enum"].([]interface{}); ok && len(enumVals) > 0 {
			enumParts := make([]string, 0, len(enumVals))
			for _, v := range enumVals {
				enumParts = append(enumParts, fmt.Sprintf("%v", v))
			}
			typeName = strings.Join(enumParts, "|")
		}
		flag := "?"
		if requiredSet[name] {
			flag = "!"
		}
		parts = append(parts, fmt.Sprintf("%s%s: %s", name, flag, typeName))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func toolArgRepairEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("TOOL_ARG_REPAIR_ENABLED")))
	return value != "0" && value != "false" && value != "off"
}

func getToolArgRepairMaxFileSize() int64 {
	const defaultMax = 512 * 1024
	value := strings.TrimSpace(os.Getenv("TOOL_ARG_REPAIR_MAX_FILE_SIZE"))
	if value == "" {
		return defaultMax
	}
	var size int64
	_, err := fmt.Sscanf(value, "%d", &size)
	if err != nil || size <= 0 {
		return defaultMax
	}
	return size
}

func buildFuzzyPattern(text string) string {
	var parts []string
	for _, ch := range text {
		switch {
		case smartDoubleQuotes[ch] || ch == '"':
			parts = append(parts, `["«“”❞‟„❝»]`)
		case smartSingleQuotes[ch] || ch == '\'':
			parts = append(parts, `['‘’‚‛]`)
		case ch == ' ' || ch == '\t':
			parts = append(parts, `\s+`)
		case ch == '\\':
			parts = append(parts, `\\{1,2}`)
		default:
			parts = append(parts, regexp.QuoteMeta(string(ch)))
		}
	}
	return strings.Join(parts, "")
}

func firstNonNil(values ...interface{}) interface{} {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func stringFromAny(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func buildOpenAIToolCalls(toolCalls []ParsedToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(toolCalls))
	for _, tc := range toolCalls {
		argJSON, _ := json.Marshal(tc.Arguments)
		out = append(out, map[string]interface{}{
			"id":   fmt.Sprintf("call_%d", time.Now().UnixNano()),
			"type": "function",
			"function": map[string]interface{}{
				"name":      tc.Name,
				"arguments": string(argJSON),
			},
		})
	}
	return out
}

func sendSSEToolCallStart(w io.Writer, id string, created int64, model string, index int, toolCallID string, name string) {
	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"tool_calls": []map[string]interface{}{
						{
							"index": index,
							"id":    toolCallID,
							"type":  "function",
							"function": map[string]interface{}{
								"name":      name,
								"arguments": "",
							},
						},
					},
				},
				"finish_reason": nil,
			},
		},
	}
	bytes, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", bytes)
	w.(http.Flusher).Flush()
}

func sendSSEToolCallArguments(w io.Writer, id string, created int64, model string, index int, arguments string) {
	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"tool_calls": []map[string]interface{}{
						{
							"index": index,
							"function": map[string]interface{}{
								"arguments": arguments,
							},
						},
					},
				},
				"finish_reason": nil,
			},
		},
	}
	bytes, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", bytes)
	w.(http.Flusher).Flush()
}
