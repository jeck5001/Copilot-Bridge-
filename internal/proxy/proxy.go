package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

type Kind string

const (
	KindDirect Kind = "direct"
	KindHTTP   Kind = "http"
	KindSOCKS5 Kind = "socks5"

	proxyDialTimeout      = 20 * time.Second
	proxyHandshakeTimeout = 20 * time.Second
	proxyCAFileEnv        = "M365_PROXY_CA_FILE"
)

type Config struct {
	Raw  string
	Type Kind
	Host string
	Port string
	User string
	Pass string
	// UseTLS is true when the user wrote `https://` for the proxy URL.
	// Many proxy providers label their service as "HTTPS" while the proxy
	// itself accepts plain HTTP CONNECT — but real TLS-wrapped proxies do
	// exist (e.g. proxy.example:443). We honor the scheme so users can pick
	// whichever the provider actually runs. This only affects how Go dials
	// the proxy host; HTTP CONNECT itself is sent as plaintext in both
	// cases, then TLS is layered on top for the target host.
	UseTLS bool
}

// Parse 识别以下格式：
//
//	http(s)://[user:pass@]host:port           (标准)
//	http(s)://host:port:user:pass             (非标准, 部分代理服务商常用)
//	socks5://[user:pass@]host:port          (标准)
//	socks5://host:port:user:pass            (非标准, 部分代理服务商常用)
//	socks5h://host:port                      (远程解析 DNS)
//	host:port                                (无 scheme, 默认按 socks5)
func Parse(raw string) (Config, error) {
	raw = strings.TrimSpace(raw)
	c := Config{Raw: raw}
	if raw == "" {
		c.Type = KindDirect
		return c, nil
	}
	low := strings.ToLower(raw)
	if i := strings.Index(low, "://"); i > 0 {
		scheme := low[:i]
		rest := raw[i+3:]
		switch scheme {
		case "http":
			c.Type = KindHTTP
			c.UseTLS = false
			return parseAuthHostPort(c, rest)
		case "https":
			c.Type = KindHTTP
			c.UseTLS = true
			return parseAuthHostPort(c, rest)
		case "socks5", "socks5h":
			c.Type = KindSOCKS5
			return parseSocks(c, rest)
		case "socks4":
			return c, fmt.Errorf("SOCKS4 is not supported; use SOCKS5")
		default:
			return c, fmt.Errorf("不支持的代理协议: %q", scheme)
		}
	}
	// 无 scheme: 默认 socks5
	c.Type = KindSOCKS5
	return parseSocks(c, raw)
}

func parseSocks(c Config, rest string) (Config, error) {
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		// 标准: user:pass@host:port
		authPart := rest[:at]
		hp := rest[at+1:]
		if co := strings.Index(authPart, ":"); co >= 0 {
			c.User = authPart[:co]
			c.Pass = authPart[co+1:]
		} else {
			c.User = authPart
		}
		return setHostPort(c, hp)
	}
	// 非标准: host:port:user:pass (恰好 4 段, 无 @)
	parts := strings.Split(rest, ":")
	if len(parts) == 4 {
		c.Host, c.Port, c.User, c.Pass = parts[0], parts[1], parts[2], parts[3]
		return validateHostPort(c)
	}
	if len(parts) == 2 {
		c.Host, c.Port = parts[0], parts[1]
		return validateHostPort(c)
	}
	return c, fmt.Errorf("无法解析 SOCKS5 代理地址")
}

func parseAuthHostPort(c Config, rest string) (Config, error) {
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		authPart := rest[:at]
		hp := rest[at+1:]
		if co := strings.Index(authPart, ":"); co >= 0 {
			c.User = authPart[:co]
			c.Pass = authPart[co+1:]
		} else {
			c.User = authPart
		}
		return setHostPort(c, hp)
	}
	// 无 @: 支持非标准 4 段 host:port:user:pass (与 SOCKS 分支一致)。
	// 例如 https://192.0.2.25:443:example-user:example-pass
	if parts := strings.Split(rest, ":"); len(parts) == 4 {
		c.Host, c.Port, c.User, c.Pass = parts[0], parts[1], parts[2], parts[3]
		return validateHostPort(c)
	}
	return setHostPort(c, rest)
}

func setHostPort(c Config, hp string) (Config, error) {
	if host, port, err := net.SplitHostPort(hp); err == nil {
		c.Host, c.Port = host, port
	} else if co := strings.LastIndex(hp, ":"); co >= 0 {
		c.Host, c.Port = hp[:co], hp[co+1:]
	} else {
		return c, fmt.Errorf("代理地址必须包含端口")
	}
	return validateHostPort(c)
}

func validateHostPort(c Config) (Config, error) {
	c.Host = strings.Trim(strings.TrimSpace(c.Host), "[]")
	c.Port = strings.TrimSpace(c.Port)
	if c.Host == "" || c.Port == "" {
		return c, fmt.Errorf("代理地址缺少 host 或 port")
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 {
		return c, fmt.Errorf("代理端口必须为 1-65535")
	}
	return c, nil
}

func (c Config) addr() string { return net.JoinHostPort(c.Host, c.Port) }

// proxyURL builds the URL Go's http transport / gorilla dialer uses to reach
// the proxy. The scheme follows the user's input: http:// → plaintext dial
// (CONNECT in HTTP), https:// → TLS dial (CONNECT over TLS). Real-world
// "HTTPS" proxies that listen on TCP/443 with TLS to the client honor this;
// providers that just label their CONNECT endpoint "HTTPS" should be entered
// as http:// instead.
func (c Config) proxyURL() *url.URL {
	u := url.URL{Scheme: "http", Host: c.addr()}
	if c.UseTLS {
		u.Scheme = "https"
	}
	if c.User != "" {
		u.User = url.UserPassword(c.User, c.Pass)
	}
	return &u
}

// tunnelDialContext opens a connection to the *target* addr by first
// establishing a tunnel through the HTTP/HTTPS proxy. Both the TLS connection
// to an HTTPS proxy and the later TLS connection to the target are verified.
func (c Config) tunnelDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, proxyHandshakeTimeout)
	defer cancel()
	var conn net.Conn
	var err error
	proxyAddr := c.addr()
	if c.UseTLS {
		roots, rootErr := x509.SystemCertPool()
		if rootErr != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if caPath := strings.TrimSpace(os.Getenv(proxyCAFileEnv)); caPath != "" {
			pem, readErr := os.ReadFile(caPath)
			if readErr != nil {
				return nil, fmt.Errorf("read proxy CA file: %w", readErr)
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("proxy CA file contains no valid certificates")
			}
		}
		d := tls.Dialer{
			NetDialer: &net.Dialer{Timeout: proxyDialTimeout},
			Config: &tls.Config{
				ServerName: c.Host,
				RootCAs:    roots,
				MinVersion: tls.VersionTLS12,
			},
		}
		conn, err = d.DialContext(dialCtx, "tcp", proxyAddr)
	} else {
		d := net.Dialer{Timeout: proxyDialTimeout}
		conn, err = d.DialContext(dialCtx, "tcp", proxyAddr)
	}
	if err != nil {
		return nil, fmt.Errorf("连接代理 %s 失败: %w", proxyAddr, err)
	}

	// Send HTTP CONNECT to the proxy.
	req, err := http.NewRequest(http.MethodConnect, "http://"+addr, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if c.User != "" {
		credential := base64.StdEncoding.EncodeToString([]byte(c.User + ":" + c.Pass))
		req.Header.Set("Proxy-Authorization", "Basic "+credential)
	}
	deadline := time.Now().Add(proxyHandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("写入代理 CONNECT 失败: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取代理响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("代理拒绝 CONNECT: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (c Config) socksAuth() *proxy.Auth {
	if c.User == "" {
		return nil
	}
	return &proxy.Auth{User: c.User, Password: c.Pass}
}

func (c Config) socksDialContext() (func(context.Context, string, string) (net.Conn, error), error) {
	forward := &net.Dialer{Timeout: proxyDialTimeout}
	d, err := proxy.SOCKS5("tcp", c.addr(), c.socksAuth(), forward)
	if err != nil {
		return nil, err
	}
	contextDialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("SOCKS5 dialer does not support context cancellation")
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, proxyDialTimeout)
		defer cancel()
		return contextDialer.DialContext(dialCtx, network, addr)
	}, nil
}

// HTTPClient 返回带代理的 *http.Client (直连返回 DefaultClient)。
func (c Config) HTTPClient() (*http.Client, error) {
	if c.Type == KindDirect {
		return http.DefaultClient, nil
	}
	switch c.Type {
	case KindHTTP:
		return &http.Client{
			Transport: &http.Transport{
				DialContext:       c.tunnelDialContext,
				Proxy:             nil,
				DisableKeepAlives: false,
			},
			Timeout: 30 * time.Second,
		}, nil
	case KindSOCKS5:
		dialContext, err := c.socksDialContext()
		if err != nil {
			return nil, err
		}
		return &http.Client{
			Transport: &http.Transport{
				DialContext: dialContext,
			},
			Timeout: 30 * time.Second,
		}, nil
	}
	return http.DefaultClient, nil
}

// WebSocketDialer 在 base 基础上注入代理, 返回新的 gorilla Dialer (不修改 base)。
func (c Config) WebSocketDialer(base *websocket.Dialer) (*websocket.Dialer, error) {
	d := *base
	if c.Type == KindDirect {
		return &d, nil
	}
	switch c.Type {
	case KindHTTP:
		d.Proxy = nil
		d.NetDialContext = c.tunnelDialContext
	case KindSOCKS5:
		dialContext, err := c.socksDialContext()
		if err != nil {
			return nil, err
		}
		d.NetDialContext = dialContext
	}
	return &d, nil
}

// HTTPClientFor 便捷封装: 直接传原始代理串。
func HTTPClientFor(raw string) (*http.Client, error) {
	c, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return c.HTTPClient()
}

// WebSocketDialerFor 便捷封装: 直接传原始代理串。
func WebSocketDialerFor(base *websocket.Dialer, raw string) (*websocket.Dialer, error) {
	c, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return c.WebSocketDialer(base)
}
