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
// Hello id: utls v1.8.2 has no HelloChrome_110 constant, and the Chrome ids
// that sit closer to 110 (112/114/115) are PSK session-resumption variants
// that must not open a first handshake. HelloChrome_106_Shuffle is
// therefore the closest first-handshake Chrome id available in v1.8.2.
//
// Known limitation of the spike: the User-Agent still claims Chrome/110
// while the TLS fingerprint is Chrome 106's. If leg (b)'s verdict turns out
// to be load-bearing for the plugin, Task 5's production transport may pick
// a different (or custom) hello to remove that mismatch.
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
