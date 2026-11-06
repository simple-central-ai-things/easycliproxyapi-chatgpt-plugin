//go:build live && utls

// The utls leg of the TLS fingerprint spike. It presents a Chrome-110-era
// ClientHello through github.com/refraction-networking/utls and speaks the
// protocol ALPN negotiates through golang.org/x/net/http2's Transport, whose
// DialTLS hook accepts any already-handshaked net.Conn.
//
// x/net/http2 is used because the stdlib's bundled http2 transport is not
// importable, and net/http.Transport cannot route HTTP/2 over a conn that is
// not a *tls.Conn (it keys the upgrade off tls.ConnectionState, which utls
// conns expose but net/http never consults for custom dialers).
//
// Hello id: utls v1.8.2 has no HelloChrome_110 constant; the pinned
// first-handshake Chrome ids jump from 106 to the PSK resumption variants
// (112/114/115), which must not be used for an initial connection. So this
// leg uses HelloChrome_106_Shuffle — the closest pinned Chrome 110 id
// (Chrome 106 already sends the shuffled-extension layout the 110 line
// kept).
package tests

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// utlsAvailable reports that this build linked the real utls leg.
func utlsAvailable() bool { return true }

// newUtlsClient returns leg (b): an http.Client whose TLS connections are
// utls-handshaken with the Chrome-110-era hello, so the wire fingerprint
// matches the reference web client rather than Go's stdlib.
func newUtlsClient() *http.Client {
	h2t := &http2.Transport{
		AllowHTTP: false,
		DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
			raw, err := net.DialTimeout(network, addr, 30*time.Second)
			if err != nil {
				return nil, err
			}
			uconn := utls.UClient(raw, nil, utls.HelloChrome_106_Shuffle)
			uconn.SetSNI("chatgpt.com")
			if err := uconn.HandshakeContext(context.Background()); err != nil {
				raw.Close()
				return nil, err
			}
			return uconn, nil
		},
	}
	return &http.Client{Transport: h2t, Timeout: liveRequestTimeout}
}
