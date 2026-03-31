package gemini

import (
	"io"
	"log"
	"strings"
	"time"
)

const (
	// IdleTimeout 两次数据块之间的最大空闲时间
	// 正常流式响应中 Gemini 每隔数百毫秒到数秒发送一次 chunk
	IdleTimeout = 60 * time.Second

	// DegenerationWindow 退化检测窗口：连续相同短文本出现此次数即中止
	DegenerationWindow = 8
	// DegenerationMaxLen 短文本判定阈值（字符数）
	DegenerationMaxLen = 20
)

// guardedReader 包装原始 ReadCloser，提供：
//  1. 空闲超时：两次成功 Read 之间超过 IdleTimeout 则关闭底层 reader
//  2. 退化循环检测：连续相同短 chunk 超过阈值则关闭底层 reader
//
// 对调用层透明——仍实现 io.ReadCloser 接口。
type guardedReader struct {
	src     io.ReadCloser
	closed  chan struct{}
	active  chan struct{} // 每次成功 Read 向此 channel 发送信号
	accID   string

	// 退化检测
	lastChunk string
	repeatCnt int
}

// NewGuardedReader 返回带空闲超时和退化检测的 ReadCloser。
// accountID 仅用于日志标识。
func NewGuardedReader(src io.ReadCloser, accountID string) io.ReadCloser {
	g := &guardedReader{
		src:    src,
		closed: make(chan struct{}),
		active: make(chan struct{}, 1),
		accID:  accountID,
	}
	go g.watchIdle()
	return g
}

func (g *guardedReader) watchIdle() {
	timer := time.NewTimer(IdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-g.active:
			// 收到活跃信号，重置计时器
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(IdleTimeout)
		case <-timer.C:
			log.Printf("[StreamGuard] 账号 '%s': 流式响应空闲超过 %v，主动关闭连接", g.accID, IdleTimeout)
			g.src.Close()
			return
		case <-g.closed:
			return
		}
	}
}

func (g *guardedReader) Read(p []byte) (int, error) {
	n, err := g.src.Read(p)
	if n > 0 {
		// 非阻塞发送活跃信号
		select {
		case g.active <- struct{}{}:
		default:
		}

		// 退化循环检测：检查读取到的内容
		chunk := strings.TrimSpace(string(p[:n]))
		if len([]rune(chunk)) <= DegenerationMaxLen && chunk != "" {
			if chunk == g.lastChunk {
				g.repeatCnt++
				if g.repeatCnt >= DegenerationWindow {
					log.Printf("[StreamGuard] 账号 '%s': 检测到退化循环（连续 %d 次相同短 chunk: %q），中止流",
						g.accID, g.repeatCnt, chunk)
					g.src.Close()
					return n, io.EOF
				}
			} else {
				g.lastChunk = chunk
				g.repeatCnt = 1
			}
		} else {
			// 长 chunk 说明正常输出，重置计数
			g.lastChunk = ""
			g.repeatCnt = 0
		}
	}
	return n, err
}

func (g *guardedReader) Close() error {
	select {
	case <-g.closed:
	default:
		close(g.closed)
	}
	return g.src.Close()
}
