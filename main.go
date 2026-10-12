// MegaFolderDownloader: 下载 MEGA 公开文件夹链接的命令行工具。
//
// 功能（复刻自实测有效的下载方案）：
//   - 解析 folder 链接，调用 MEGA API 列出全部节点并解密文件名/密钥
//   - 多文件并行下载
//   - 大文件自动分段并行下载（多连接绕过单连接限速），分段可断点续传
//   - 下载中断/停滞自动换新链接重试
//
// 仅使用 Go 标准库。
package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const apiBase = "https://g.api.mega.co.nz/cs"
const mib = 1048576

// ---------- 基础工具 ----------

func b64dec(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func ecbDecrypt(key, data []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	bs := block.BlockSize()
	out := make([]byte, len(data))
	for i := 0; i+bs <= len(data); i += bs {
		block.Decrypt(out[i:i+bs], data[i:i+bs])
	}
	return out
}

func decryptAttributes(at string, key []byte) (string, error) {
	raw, err := b64dec(at)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	if len(raw)%block.BlockSize() != 0 {
		return "", fmt.Errorf("属性长度异常")
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, make([]byte, 16)).CryptBlocks(plain, raw)
	plain = bytes.TrimRight(plain, "\x00")
	plain = bytes.TrimPrefix(plain, []byte("MEGA"))
	var attrs struct {
		N string `json:"n"`
	}
	if err := json.Unmarshal(plain, &attrs); err != nil {
		return "", err
	}
	return attrs.N, nil
}

// sanitizeName 把文件名清洗为 Windows/通用文件系统安全的名字。
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimRight(b.String(), " .")
	if s == "" {
		s = "_"
	}
	return s
}

// ---------- MEGA 客户端 ----------

type Node struct {
	Handle string
	Parent string
	Type   int // 0=文件 1=文件夹
	Size   int64
	Key    []byte // 文件: 16B AES 密钥; 文件夹: 16B
	Nonce  []byte // 文件: 8B
	Name   string
}

type Client struct {
	handle string
	master []byte
	hc     *http.Client
	seq    atomic.Int64
}

func parseFolderURL(raw string) (handle, key string, err error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "#F!"); i >= 0 { // 旧式链接 #F!handle!key
		parts := strings.Split(raw[i+3:], "!")
		if len(parts) >= 2 {
			return parts[0], strings.Split(parts[1], "/")[0], nil
		}
		return "", "", fmt.Errorf("无法解析旧式文件夹链接")
	}
	idx := strings.Index(raw, "/folder/")
	if idx < 0 {
		return "", "", fmt.Errorf("这不是 MEGA 文件夹链接（缺少 /folder/）")
	}
	rest := raw[idx+len("/folder/"):]
	hash := strings.Index(rest, "#")
	if hash < 0 {
		return "", "", fmt.Errorf("链接缺少密钥（# 后面的部分）")
	}
	handle = strings.Split(rest[:hash], "/")[0]
	key = strings.Split(rest[hash+1:], "/")[0]
	if handle == "" || key == "" {
		return "", "", fmt.Errorf("链接不完整")
	}
	return handle, key, nil
}

func NewClient(folderURL string, tr *http.Transport) (*Client, error) {
	handle, keyB64, err := parseFolderURL(folderURL)
	if err != nil {
		return nil, err
	}
	master, err := b64dec(keyB64)
	if err != nil {
		return nil, fmt.Errorf("密钥解码失败: %w", err)
	}
	if len(master) != 16 {
		return nil, fmt.Errorf("密钥长度异常: %d", len(master))
	}
	return &Client{
		handle: handle,
		master: master,
		hc:     &http.Client{Timeout: 30 * time.Second, Transport: tr},
	}, nil
}

// ---------- 代理 ----------

// normalizeProxy 把用户/系统给出的代理地址补全为 URL。
// 接受 "127.0.0.1:7890"、"http://127.0.0.1:7890"、"socks5://127.0.0.1:1080"，
// 以及系统代理的 "http=...;https=...;socks=..." 分段格式。
func normalizeProxy(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.Contains(s, "=") { // 分段格式：优先 https，其次 http
		parts := strings.Split(s, ";")
		pick := ""
		for _, want := range []string{"https=", "http="} {
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(strings.ToLower(p), want) {
					pick = strings.TrimSpace(p[len(want):])
					break
				}
			}
			if pick != "" {
				break
			}
		}
		if pick == "" {
			pick = strings.TrimSpace(parts[0])
			if i := strings.Index(pick, "="); i >= 0 {
				pick = pick[i+1:]
			}
		}
		s = pick
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return url.Parse(s)
}

// resolveProxy 按优先级决定代理：-proxy 参数 > 环境变量 > 系统代理（浏览器同款）。
// 返回 (代理URL或nil, 来源说明)。
func resolveProxy(flagVal string) (*url.URL, string) {
	if strings.TrimSpace(flagVal) != "" {
		if u, err := normalizeProxy(flagVal); err == nil && u != nil {
			return u, "命令行参数"
		}
	}
	for _, env := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := os.Getenv(env); strings.TrimSpace(v) != "" {
			if u, err := normalizeProxy(v); err == nil && u != nil {
				return u, "环境变量 " + env
			}
		}
	}
	if v := systemProxy(); strings.TrimSpace(v) != "" {
		if u, err := normalizeProxy(v); err == nil && u != nil {
			return u, "系统代理（浏览器同款）"
		}
	}
	return nil, ""
}

func buildTransport(proxy *url.URL) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
	} else {
		tr.Proxy = nil // 直连（不走 ProxyFromEnvironment，已在 resolveProxy 处理）
	}
	return tr
}

// api 调用 MEGA 接口，返回响应数组的第一个元素。负数错误码会重试（-3 为临时错误）。
func (c *Client) api(payload map[string]any) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		id := c.seq.Add(1) + time.Now().Unix()%1000000
		url := fmt.Sprintf("%s?id=%d&n=%s", apiBase, id, c.handle)
		body, _ := json.Marshal([]map[string]any{payload})
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil || len(arr) == 0 {
			lastErr = fmt.Errorf("接口返回异常: %s", truncate(string(data), 120))
			time.Sleep(time.Second)
			continue
		}
		first := bytes.TrimSpace(arr[0])
		if len(first) > 0 && (first[0] == '-' || (first[0] >= '0' && first[0] <= '9')) {
			var code int
			if err := json.Unmarshal(first, &code); err == nil {
				if code == -3 { // EAGAIN，稍后重试
					lastErr = fmt.Errorf("MEGA 接口临时错误 -3")
					time.Sleep(2 * time.Second)
					continue
				}
				return nil, fmt.Errorf("MEGA 接口错误码 %d", code)
			}
		}
		return arr[0], nil
	}
	return nil, fmt.Errorf("接口多次重试失败: %w", lastErr)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type rawNode struct {
	H  string `json:"h"`
	P  string `json:"p"`
	T  int    `json:"t"`
	S  int64  `json:"s"`
	K  string `json:"k"`
	At string `json:"at"`
	A  string `json:"a"`
}

func (c *Client) ListNodes() (map[string]*Node, error) {
	raw, err := c.api(map[string]any{"a": "f", "c": 1, "r": 1})
	if err != nil {
		return nil, err
	}
	var resp struct {
		F []rawNode `json:"f"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	nodes := make(map[string]*Node, len(resp.F))
	for _, rn := range resp.F {
		n := &Node{Handle: rn.H, Parent: rn.P, Type: rn.T, Size: rn.S}
		if rn.T != 0 && rn.T != 1 {
			n.Name = rn.H
			nodes[rn.H] = n
			continue
		}
		kParts := strings.Split(rn.K, "/")
		kStr := strings.Split(kParts[len(kParts)-1], ":")
		kEnc, err := b64dec(kStr[len(kStr)-1])
		if err != nil {
			return nil, fmt.Errorf("节点 %s 密钥解码失败: %w", rn.H, err)
		}
		kDec := ecbDecrypt(c.master, kEnc)
		var attrKey []byte
		if rn.T == 0 {
			if len(kDec) < 32 {
				return nil, fmt.Errorf("节点 %s 密钥长度异常", rn.H)
			}
			w := func(i int) uint32 { return binary.BigEndian.Uint32(kDec[i*4:]) }
			key := make([]byte, 16)
			for i := 0; i < 4; i++ {
				binary.BigEndian.PutUint32(key[i*4:], w(i)^w(i+4))
			}
			n.Key = key
			n.Nonce = append([]byte(nil), kDec[16:24]...)
			attrKey = key
		} else {
			if len(kDec) < 16 {
				return nil, fmt.Errorf("节点 %s 密钥长度异常", rn.H)
			}
			n.Key = append([]byte(nil), kDec[:16]...)
			attrKey = n.Key
		}
		at := rn.At
		if at == "" {
			at = rn.A
		}
		name, err := decryptAttributes(at, attrKey)
		if err != nil {
			return nil, fmt.Errorf("节点 %s 名称解密失败: %w", rn.H, err)
		}
		n.Name = name
		nodes[rn.H] = n
	}
	return nodes, nil
}

// relPath 构造节点在共享文件夹内的相对路径（包含顶层文件夹名）。
func relPath(nodes map[string]*Node, n *Node) string {
	parts := []string{}
	cur := n
	for cur != nil {
		parts = append(parts, sanitizeName(cur.Name))
		parent, ok := nodes[cur.Parent]
		if !ok {
			break
		}
		cur = parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return filepath.Join(parts...)
}

func (c *Client) downloadURL(handle string) (string, error) {
	raw, err := c.api(map[string]any{"a": "g", "g": 1, "n": handle})
	if err != nil {
		return "", err
	}
	var resp struct {
		G string `json:"g"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if resp.G == "" {
		return "", fmt.Errorf("未取得下载地址（可能被限流）")
	}
	return resp.G, nil
}

// ---------- 下载引擎 ----------

type Engine struct {
	client   *Client
	hc       *http.Client
	connSem  chan struct{}
	attempts int
	stallSec int

	mu       sync.Mutex
	active   map[string]struct{} // 正在写入的临时文件（用于统计进度）
	finished atomic.Int64        // 已完成文件字节数
	filesOK  atomic.Int64
}

func NewEngine(c *Client, hc *http.Client, conns, attempts, stallSec int) *Engine {
	return &Engine{
		client:   c,
		hc:       hc,
		connSem:  make(chan struct{}, conns),
		attempts: attempts,
		stallSec: stallSec,
		active:   make(map[string]struct{}),
	}
}

func (e *Engine) track(path string)   { e.mu.Lock(); e.active[path] = struct{}{}; e.mu.Unlock() }
func (e *Engine) untrack(path string) { e.mu.Lock(); delete(e.active, path); e.mu.Unlock() }

func (e *Engine) partialBytes() int64 {
	e.mu.Lock()
	paths := make([]string, 0, len(e.active))
	for p := range e.active {
		paths = append(paths, p)
	}
	e.mu.Unlock()
	var sum int64
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			sum += st.Size()
		}
	}
	return sum
}

func ctrStream(key, nonce []byte, blockIndex uint64) cipher.Stream {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	iv := make([]byte, 16)
	copy(iv, nonce)
	binary.BigEndian.PutUint64(iv[8:], blockIndex)
	return cipher.NewCTR(block, iv)
}

// stallBody 在 stallSec 秒无数据时关闭连接，使上层读出错并触发重试。
type stallBody struct {
	rc      io.ReadCloser
	last    atomic.Int64
	done    atomic.Bool
	timeout time.Duration
	once    sync.Once
}

func newStallBody(rc io.ReadCloser, timeout time.Duration) *stallBody {
	s := &stallBody{rc: rc, timeout: timeout}
	s.last.Store(time.Now().UnixNano())
	go func() {
		for !s.done.Load() {
			time.Sleep(2 * time.Second)
			if time.Since(time.Unix(0, s.last.Load())) > s.timeout {
				s.once.Do(func() { rc.Close() })
				return
			}
		}
	}()
	return s
}

func (s *stallBody) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (s *stallBody) Close() error {
	s.done.Store(true)
	return s.rc.Close()
}

// ---------- 509 限流处理 ----------
//
// MEGA 的免费流量配额按出口 IP 计算，超限时下载服务器返回 HTTP 509。
// 命中后全局暂停一段时间再自动继续，已下载的分段/文件保留续传，
// 不会把 509 计入失败重试次数。

var errQuota = errors.New("MEGA 流量配额已用完（HTTP 509）")

var quotaPauseSeconds = 300 // 可由 -quota-pause 修改

var (
	quotaMu       sync.Mutex
	quotaResumeAt time.Time
	quotaNoticeAt time.Time
)

func noteQuota() {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	now := time.Now()
	pause := time.Duration(quotaPauseSeconds) * time.Second
	if quotaResumeAt.Before(now.Add(pause - time.Minute)) {
		quotaResumeAt = now.Add(pause)
	}
	if now.Sub(quotaNoticeAt) > time.Minute {
		quotaNoticeAt = now
		fmt.Printf("\n[MEGA 限流] 当前出口 IP 的免费流量配额已用完（509），全体下载已暂停，将在 %s 自动继续。已下载内容会保留续传；也可以换个代理节点（换出口 IP）后重新运行。\n", quotaResumeAt.Format("15:04:05"))
	}
}

func waitQuota() {
	for {
		quotaMu.Lock()
		until := quotaResumeAt
		quotaMu.Unlock()
		if time.Until(until) <= 0 {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

// getURL 取得下载地址；若接口因配额返回 509，等待配额窗口后自动重试。
func (e *Engine) getURL(node *Node) (string, error) {
	for i := 0; i < 200; i++ {
		waitQuota()
		url, err := e.client.downloadURL(node.Handle)
		if err == nil {
			return url, nil
		}
		if strings.Contains(err.Error(), "509") {
			noteQuota()
			continue
		}
		return "", err
	}
	return "", errQuota
}
func (e *Engine) fetchRange(node *Node, url string, start, end int64, w io.Writer) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	ranged := start > 0 || end >= 0
	if ranged {
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		}
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return err
	}
	body := newStallBody(resp.Body, time.Duration(e.stallSec)*time.Second)
	defer body.Close()
	if resp.StatusCode == 509 {
		return errQuota
	}
	if ranged && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("服务器未返回分段内容: %s", resp.Status)
	}
	if !ranged && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器返回: %s", resp.Status)
	}
	stream := ctrStream(node.Key, node.Nonce, uint64(start/16))
	_, err = io.CopyBuffer(w, cipher.StreamReader{S: stream, R: body}, make([]byte, 1<<18))
	return err
}

// downloadDirect 单连接下载整个文件（写 .part，成功后改名）。
func (e *Engine) downloadDirect(node *Node, dest string) error {
	if st, err := os.Stat(dest); err == nil && st.Size() == node.Size {
		return nil
	}
	part := dest + ".part"
	if st, err := os.Stat(part); err == nil && st.Size() == node.Size {
		return os.Rename(part, dest) // 上次已下完、只是没改名
	}
	var lastErr error
	for a := 1; a <= e.attempts; {
		url, err := e.getURL(node)
		if err != nil {
			lastErr = err
			a++
			time.Sleep(2 * time.Second)
			continue
		}
		// 断点续传：已有 .part 时从其末尾（16 对齐）处继续，不整文件重下
		var offset int64
		if st, serr := os.Stat(part); serr == nil && st.Size() > 0 && st.Size() < node.Size {
			offset = st.Size() - st.Size()%16
		}
		e.track(part)
		f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			if err = f.Truncate(offset); err == nil {
				_, err = f.Seek(offset, io.SeekStart)
			}
			if err == nil {
				e.connSem <- struct{}{}
				err = e.fetchRange(node, url, offset, -1, f)
				<-e.connSem
			}
			f.Close()
		}
		e.untrack(part)
		if errors.Is(err, errQuota) {
			noteQuota()
			continue // 保留 .part，配额恢复后从断点继续
		}
		if err == nil {
			if st, serr := os.Stat(part); serr == nil && st.Size() == node.Size {
				return os.Rename(part, dest)
			}
			err = fmt.Errorf("大小不符")
		}
		lastErr = err
		a++
		time.Sleep(time.Duration(a) * time.Second)
	}
	return lastErr
}

// downloadSegmented 分段并行下载：分段存为独立临时文件，可断点续传；
// 全部就绪后按序拼接为成品文件。
func (e *Engine) downloadSegmented(node *Node, dest string, nseg int) error {
	if st, err := os.Stat(dest); err == nil && st.Size() == node.Size {
		return nil
	}
	segSize := (node.Size/int64(nseg)/65536 + 1) * 65536
	type seg struct {
		idx        int
		start, end int64
		path       string
	}
	var segs []seg
	for start, i := int64(0), 0; start < node.Size; start, i = start+segSize, i+1 {
		end := start + segSize - 1
		if end >= node.Size {
			end = node.Size - 1
		}
		segs = append(segs, seg{i, start, end, fmt.Sprintf("%s.seg%d", dest, i)})
	}
	errCh := make(chan error, len(segs))
	var wg sync.WaitGroup
	for _, sg := range segs {
		sg := sg
		want := sg.end - sg.start + 1
		if st, err := os.Stat(sg.path); err == nil && st.Size() == want {
			e.finished.Add(0) // 已就绪的分段在进度统计中由文件大小体现
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lastErr error
			for a := 1; a <= e.attempts; {
				url, err := e.getURL(node)
				if err != nil {
					lastErr = err
					a++
					time.Sleep(2 * time.Second)
					continue
				}
				// 断点续传：分段已下了一部分时，从已有字节（16 对齐）处接着下
				var have int64
				if st, serr := os.Stat(sg.path); serr == nil && st.Size() > 0 && st.Size() < want {
					have = st.Size() - st.Size()%16
				}
				e.track(sg.path)
				f, err := os.OpenFile(sg.path, os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					if err = f.Truncate(have); err == nil {
						_, err = f.Seek(have, io.SeekStart)
					}
					if err == nil {
						e.connSem <- struct{}{}
						err = e.fetchRange(node, url, sg.start+have, sg.end, f)
						<-e.connSem
					}
					f.Close()
				}
				// 注意：分段路径保留在 active 中直到整个文件完成，进度统计才准确
				if errors.Is(err, errQuota) {
					noteQuota()
					continue // 保留已下部分，配额恢复后从断点继续
				}
				if err == nil {
					if st, serr := os.Stat(sg.path); serr == nil && st.Size() == want {
						return
					}
					err = fmt.Errorf("分段大小不符")
				}
				lastErr = err
				a++
				time.Sleep(time.Duration(a) * time.Second)
			}
			errCh <- fmt.Errorf("分段 %d: %w", sg.idx, lastErr)
		}()
	}
	wg.Wait()
	close(errCh)
	defer func() {
		for _, sg := range segs {
			e.untrack(sg.path)
		}
	}()
	for err := range errCh {
		return err
	}
	// 拼接
	tmp := dest + ".assembled"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, sg := range segs {
		in, err := os.Open(sg.path)
		if err != nil {
			out.Close()
			return err
		}
		if _, err := io.CopyBuffer(out, in, make([]byte, 1<<20)); err != nil {
			in.Close()
			out.Close()
			return err
		}
		in.Close()
	}
	if err := out.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() != node.Size {
		os.Remove(tmp)
		return fmt.Errorf("拼接后大小不符")
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	for _, sg := range segs {
		os.Remove(sg.path)
	}
	return nil
}

// ---------- 主流程 ----------

type job struct {
	node *Node
	rel  string
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func main() {
	var (
		outDir     = flag.String("o", ".", "保存目录")
		listOnly   = flag.Bool("list", false, "只列出文件，不下载")
		workers    = flag.Int("workers", 4, "同时下载的文件数")
		conns      = flag.Int("conns", 10, "最大同时连接数（限速绕行的关键参数）")
		nseg       = flag.Int("segments", 6, "大文件的分段数")
		segThresh  = flag.Int64("seg-threshold", 32*mib, "超过该大小的文件使用分段下载（字节）")
		attempts   = flag.Int("attempts", 4, "每段/每文件的重试次数")
		stallSec   = flag.Int("stall", 45, "无数据多少秒判定停滞并重试")
		only       = flag.String("only", "", "只处理路径包含该字符串的文件")
		maxFiles   = flag.Int("max-files", 0, "最多处理 N 个文件（0=不限，测试用）")
		proxyFlag  = flag.String("proxy", "", "代理地址，如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080；留空则自动读取系统代理（与浏览器相同）")
		quotaPause = flag.Int("quota-pause", 300, "遇到 MEGA 限流（509）时暂停的秒数，暂停后自动继续")
	)
	flag.Parse()
	rest := flag.Args()
	folderURL := ""
	if len(rest) > 0 {
		folderURL = rest[0]
		if len(rest) > 1 {
			// 允许把参数写在链接后面，例如: 程序 <链接> -o D:\Downloads -segments 8
			_ = flag.CommandLine.Parse(rest[1:])
		}
	}

	interactive := false
	if folderURL == "" {
		interactive = true
		fmt.Println("=== MEGA 文件夹下载器 ===")
		fmt.Print("请粘贴 MEGA 文件夹链接: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		folderURL = strings.TrimSpace(line)
		if folderURL == "" {
			fmt.Println("未输入链接，退出。")
			return
		}
		fmt.Print("保存到（回车 = 当前目录下 downloads 文件夹）: ")
		dir, _ := reader.ReadString('\n')
		dir = strings.TrimSpace(dir)
		if dir == "" {
			dir = "downloads"
		}
		*outDir = dir
	}

	quotaPauseSeconds = *quotaPause

	proxyURL, proxyFrom := resolveProxy(*proxyFlag)
	transport := buildTransport(proxyURL)
	if proxyURL != nil {
		fmt.Printf("使用代理: %s（来源: %s）\n", proxyURL.Redacted(), proxyFrom)
	} else {
		fmt.Println("未检测到代理，使用直连（如需代理: 打开你的代理软件的“系统代理”，或加参数 -proxy http://127.0.0.1:7890）")
	}

	client, err := NewClient(folderURL, transport)
	if err != nil {
		fmt.Println("链接解析失败:", err)
		waitExit(interactive)
		os.Exit(1)
	}
	fmt.Println("正在获取文件列表...")
	nodes, err := client.ListNodes()
	if err != nil {
		fmt.Println("获取文件列表失败:", err)
		waitExit(interactive)
		os.Exit(1)
	}

	var jobs []job
	var total int64
	for _, n := range nodes {
		if n.Type != 0 {
			continue
		}
		rel := relPath(nodes, n)
		if *only != "" && !strings.Contains(rel, *only) {
			continue
		}
		jobs = append(jobs, job{n, rel})
		total += n.Size
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].rel < jobs[j].rel })

	fmt.Printf("共 %d 个文件，总大小 %s\n", len(jobs), humanSize(total))
	for _, j := range jobs {
		fmt.Printf("  %s  (%s)\n", j.rel, humanSize(j.node.Size))
	}
	if *listOnly {
		waitExit(interactive)
		return
	}
	if *maxFiles > 0 && len(jobs) > *maxFiles {
		jobs = jobs[:*maxFiles]
		total = 0
		for _, j := range jobs {
			total += j.node.Size
		}
		fmt.Printf("（测试模式：只下载前 %d 个文件）\n", len(jobs))
	}

	engine := NewEngine(client, &http.Client{Transport: transport}, *conns, *attempts, *stallSec)

	// 进度报告
	doneFlag := atomic.Bool{}
	go func() {
		start := time.Now()
		var lastBytes int64
		var lastTime = time.Now()
		for !doneFlag.Load() {
			time.Sleep(3 * time.Second)
			done := engine.finished.Load() + engine.partialBytes()
			now := time.Now()
			speed := float64(done-lastBytes) / now.Sub(lastTime).Seconds()
			lastBytes, lastTime = done, now
			pct := 0.0
			if total > 0 {
				pct = float64(done) * 100 / float64(total)
			}
			fmt.Printf("\r进度 %.1f%%  %s / %s  速度 %.2f MB/s  已用 %s   ",
				pct, humanSize(done), humanSize(total), speed/1e6,
				time.Since(start).Round(time.Second))
		}
	}()

	jobCh := make(chan job)
	var wg sync.WaitGroup
	var failMu sync.Mutex
	var failures []string
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				dest := filepath.Join(*outDir, j.rel)
				if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
					failMu.Lock()
					failures = append(failures, j.rel+": "+err.Error())
					failMu.Unlock()
					continue
				}
				var err error
				if j.node.Size >= *segThresh {
					err = engine.downloadSegmented(j.node, dest, *nseg)
				} else {
					err = engine.downloadDirect(j.node, dest)
				}
				if err != nil {
					failMu.Lock()
					failures = append(failures, j.rel+": "+err.Error())
					failMu.Unlock()
					fmt.Printf("\n失败: %s (%v)\n", j.rel, err)
				} else {
					engine.finished.Add(j.node.Size)
					engine.filesOK.Add(1)
					fmt.Printf("\n完成: %s (%s)\n", j.rel, humanSize(j.node.Size))
				}
			}
		}()
	}
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()
	doneFlag.Store(true)

	fmt.Printf("\n全部结束：成功 %d / %d 个文件", engine.filesOK.Load(), len(jobs))
	if len(failures) > 0 {
		fmt.Printf("，失败 %d 个：\n", len(failures))
		for _, f := range failures {
			fmt.Println("  -", f)
		}
		fmt.Println("重新运行同一命令即可续传未完成的部分。")
	} else {
		fmt.Println("，全部成功。")
	}
	waitExit(interactive)
	if len(failures) > 0 {
		os.Exit(2)
	}
}

func waitExit(interactive bool) {
	if interactive {
		fmt.Print("\n按回车退出...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
}
