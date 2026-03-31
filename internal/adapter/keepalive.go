package adapter

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// KeepaliveInterval SSE 保活注释行的发送间隔
	// 大多数反向代理（Nginx/Caddy）默认 60~75s 超时，15s 足够保活
	KeepaliveInterval = 15 * time.Second
)

// StartSSEKeepalive 在后台 goroutine 中每隔 KeepaliveInterval 向 w 写入
// SSE 注释行（: keepalive\n\n），防止反向代理因无数据传输而断开连接。
//
// 调用方在流结束后关闭 done channel 即可停止 keepalive goroutine：
//
//	done := make(chan struct{})
//	go StartSSEKeepalive(w, done)
//	// ... 流式写入 ...
//	close(done)
func StartSSEKeepalive(w io.Writer, done <-chan struct{}) {
	ticker := time.NewTicker(KeepaliveInterval)
	defer ticker.Stop()
	flusher, canFlush := w.(http.Flusher)
	for {
		select {
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			if canFlush {
				flusher.Flush()
			}
		case <-done:
			return
		}
	}
}
