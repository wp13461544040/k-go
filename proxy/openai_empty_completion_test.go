package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestIsEmptyOpenAICompletion 锁定"空完成"的判定口径：
// 没有可见正文、没有工具调用才算空；reasoning 作为正文输出时客户端能看到内容，不算空。
func TestIsEmptyOpenAICompletion(t *testing.T) {
	cases := []struct {
		name                  string
		visibleContent        string
		toolCallCount         int
		clientSawThinkingText bool
		want                  bool
	}{
		{"只有思考、无正文无工具", "", 0, false, true},
		{"空白正文", "   \n\t", 0, false, true},
		{"有正文", "hello", 0, false, false},
		{"有工具调用", "", 1, false, false},
		{"thinking 作为正文输出", "", 0, true, false},
	}
	for _, c := range cases {
		if got := isEmptyOpenAICompletion(c.visibleContent, c.toolCallCount, c.clientSawThinkingText); got != c.want {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestOpenAIEmptyStreamErrorPayload 校验流内错误事件的形状。
// 客户端（DSH pi-ai）依据 message 中的 "stream ended without" 把失败归类为
// TRANSPORT 并自动重试，所以这段措辞不能被随意改动。
func TestOpenAIEmptyStreamErrorPayload(t *testing.T) {
	payload := openAIEmptyStreamError()
	var decoded struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if decoded.Error.Code != "empty_response" {
		t.Fatalf("code = %q, want empty_response", decoded.Error.Code)
	}
	if !strings.Contains(decoded.Error.Message, "stream ended without") {
		t.Fatalf("message = %q, 需要包含 \"stream ended without\" 才能触发客户端 TRANSPORT 归类", decoded.Error.Message)
	}
}
