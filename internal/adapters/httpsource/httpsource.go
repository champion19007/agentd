// Package httpsource is a driven adapter implementing ports.Source over HTTP.
package httpsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Options configure a Source.
type Options struct {
	// Timeout bounds one fetch. Zero means DefaultTimeout.
	Timeout time.Duration

	// ConnectTimeout bounds connection establishment (TCP/TLS handshake). Zero means DefaultConnectTimeout.
	ConnectTimeout time.Duration

	// MaxBytes caps how much of a response is read. Zero means DefaultMaxBytes.
	// The cap exists because a check pointed at something enormous should fail
	// politely rather than exhaust memory.
	MaxBytes int64

	// UserAgent identifies Agentd to the source. Being identifiable is the
	// courteous default for something that fetches a page on a schedule.
	UserAgent string

	// AllowPrivateIPs permits requests to private, loopback, and link-local addresses.
	// False by default for SSRF protection.
	AllowPrivateIPs bool

	// Client overrides the HTTP client, for tests.
	Client *http.Client
}

const (
	// DefaultTimeout bounds one fetch.
	DefaultTimeout = 30 * time.Second
	// DefaultConnectTimeout bounds the initial TCP connection.
	DefaultConnectTimeout = 10 * time.Second
	// DefaultMaxBytes caps a response at 8 MiB.
	DefaultMaxBytes = 8 << 20
	// DefaultUserAgent identifies Agentd.
	DefaultUserAgent = "agentd/1 (+https://github.com/champion19007/agentd)"
)

// ssrfError is returned when a connection target resolves to a forbidden network address.
type ssrfError struct {
	host string
	ip   string
}

func (e *ssrfError) Error() string {
	return fmt.Sprintf("ssrf: blocked request to private or local address %s (%s)", e.host, e.ip)
}

var privateIPBlocks []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"169.254.0.0/16", // IPv4 link-local
		"100.64.0.0/10",  // Shared address space (CGNAT)
		"0.0.0.0/8",      // Current network
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local (private)
		"fe80::/10",      // IPv6 link-local
		"::/128",         // IPv6 unspecified
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPBlocks = append(privateIPBlocks, block)
		}
	}
}

// isPrivateOrLocal reports whether an IP address belongs to loopback, private,
// or link-local ranges.
func isPrivateOrLocal(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// Source fetches over HTTP.
type Source struct {
	client          *http.Client
	maxBytes        int64
	userAgent       string
	allowPrivateIPs bool
}

var _ ports.Source = (*Source)(nil)

// New builds a Source.
func New(opts Options) *Source {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	client := opts.Client
	if client == nil {
		dialer := &net.Dialer{
			Timeout:   connectTimeout,
			KeepAlive: 30 * time.Second,
		}
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if !opts.AllowPrivateIPs {
					if ip := net.ParseIP(host); ip != nil {
						if isPrivateOrLocal(ip) {
							return nil, &ssrfError{host: host, ip: ip.String()}
						}
					} else {
						ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
						if err != nil {
							return nil, err
						}
						if len(ips) == 0 {
							return nil, fmt.Errorf("no IP address found for %s", host)
						}
						for _, ip := range ips {
							if isPrivateOrLocal(ip) {
								return nil, &ssrfError{host: host, ip: ip.String()}
							}
						}
						// Pin connection to first verified IP to protect against DNS rebinding
						addr = net.JoinHostPort(ips[0].String(), port)
					}
				}
				return dialer.DialContext(ctx, network, addr)
			},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}

		client = &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				if err := checkURL(req.URL.String(), opts.AllowPrivateIPs); err != nil {
					return err
				}
				return nil
			},
		}
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	return &Source{
		client:          client,
		maxBytes:        maxBytes,
		userAgent:       ua,
		allowPrivateIPs: opts.AllowPrivateIPs,
	}
}

// Fetch retrieves the source.
func (s *Source) Fetch(ctx context.Context, spec domain.SourceSpec, secrets domain.SecretBundle) (domain.RawResponse, error) {
	if spec.Kind != domain.SourceHTTP {
		return domain.RawResponse{}, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "wrong_adapter",
			Summary: fmt.Sprintf("this check is a %q source and cannot be fetched over HTTP", spec.Kind),
		}
	}
	if err := checkURL(spec.URL, s.allowPrivateIPs); err != nil {
		return domain.RawResponse{}, err
	}

	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, spec.URL, nil)
	if err != nil {
		return domain.RawResponse{}, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "bad_request",
			Summary: "this check's address could not be turned into a request",
			Detail:  err.Error(),
		}
	}

	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept", "*/*")
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	// Secret-backed headers last, so a plain header cannot shadow one.
	for header, ref := range spec.SecretHeaders {
		secret, ok := secrets.Get(ref)
		if !ok {
			return domain.RawResponse{}, domain.Failure{
				Class:   domain.ClassAuth,
				Code:    "secret_missing",
				Summary: fmt.Sprintf("this check needs a credential named %q that was not supplied", ref),
			}
		}
		req.Header.Set(header, secret.Reveal())
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return domain.RawResponse{}, classifyTransport(err)
	}
	defer resp.Body.Close()

	if f := classifyStatus(resp); f != nil {
		return domain.RawResponse{}, *f
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBytes+1))
	if err != nil {
		return domain.RawResponse{}, domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "read_failed",
			Summary: "the connection dropped while reading the source",
			Detail:  err.Error(),
		}
	}
	if int64(len(body)) > s.maxBytes {
		return domain.RawResponse{}, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "too_large",
			Summary: fmt.Sprintf("the source is larger than the %d byte limit this check allows", s.maxBytes),
		}
	}

	return domain.RawResponse{
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
		Status:      resp.StatusCode,
		FetchedAt:   time.Now().UTC(),
	}, nil
}

// checkURL rejects addresses Agentd will not fetch.
func checkURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "bad_url",
			Summary: "this check's address is not a valid URL",
			Detail:  err.Error(),
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "bad_scheme",
			Summary: fmt.Sprintf("Agentd fetches http and https sources; %q is neither", u.Scheme),
		}
	}
	if u.Host == "" {
		return domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "bad_url",
			Summary: "this check's address has no host",
		}
	}

	if !allowPrivate {
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return domain.Failure{
				Class:   domain.ClassFatal,
				Code:    "ssrf_blocked",
				Summary: "requests to localhost or private/loopback addresses are forbidden by default",
			}
		}
		if ip := net.ParseIP(host); ip != nil && isPrivateOrLocal(ip) {
			return domain.Failure{
				Class:   domain.ClassFatal,
				Code:    "ssrf_blocked",
				Summary: fmt.Sprintf("requests to private, loopback or link-local address %s are forbidden", ip),
			}
		}
	}
	return nil
}

// classifyTransport turns a connection-level error into a failure class.
func classifyTransport(err error) domain.Failure {
	var ssrf *ssrfError
	if errors.As(err, &ssrf) || strings.Contains(err.Error(), "ssrf:") {
		return domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "ssrf_blocked",
			Summary: "requests to private, loopback or link-local addresses are forbidden by default",
			Detail:  err.Error(),
		}
	}

	switch {
	case errors.Is(err, context.Canceled):
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "cancelled",
			Summary: "the check was stopped before the source answered",
			Detail:  err.Error(),
		}
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "timeout",
			Summary: "the source did not answer in time",
			Detail:  err.Error(),
		}
	}

	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return domain.Failure{
				Class:   domain.ClassTransient,
				Code:    "dns_not_found",
				Summary: "the source's address could not be found",
				Detail:  err.Error(),
			}
		}
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "dns_failed",
			Summary: "the source's address could not be looked up",
			Detail:  err.Error(),
		}
	}

	if strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "certificate") {
		return domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "tls_failed",
			Summary: "the source's security certificate could not be verified",
			Detail:  err.Error(),
		}
	}

	return domain.Failure{
		Class:   domain.ClassTransient,
		Code:    "connect_failed",
		Summary: "the source could not be reached",
		Detail:  err.Error(),
	}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// classifyStatus turns an HTTP status into a failure, or nil when the response
// is usable.
func classifyStatus(resp *http.Response) *domain.Failure {
	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return nil

	case code == http.StatusTooManyRequests:
		f := domain.Failure{
			Class:   domain.ClassRateLimited,
			Code:    "rate_limited",
			Summary: "the source asked Agentd to slow down",
		}
		if after := retryAfter(resp); after > 0 {
			f.Summary = fmt.Sprintf("the source asked Agentd to wait %s before trying again", after)
			f.Detail = after.String()
		}
		return &f

	case code == http.StatusUnauthorized, code == http.StatusForbidden,
		code == http.StatusProxyAuthRequired:
		return &domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "unauthorised",
			Summary: fmt.Sprintf("the source refused Agentd's credentials (HTTP %d)", code),
		}

	case code == http.StatusNotFound, code == http.StatusGone:
		return &domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "not_found",
			Summary: "the source is no longer at this address",
		}

	case code >= 500:
		return &domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "server_error",
			Summary: fmt.Sprintf("the source is having trouble of its own (HTTP %d)", code),
		}

	default:
		return &domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "http_" + strconv.Itoa(code),
			Summary: fmt.Sprintf("the source answered with HTTP %d", code),
		}
	}
}

// retryAfter reads the Retry-After header in either of its forms.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}
