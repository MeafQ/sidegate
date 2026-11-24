// labweb is the lab's internet-side target. It reports the source address it
// sees, so a check can tell which exit a connection used. It also stands in for
// the subscription feeds and for GitHub's releases.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var zeros = make([]byte, 256<<10)

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ip", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprintln(w, host)
	})
	mux.HandleFunc("/bytes", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "n must be a byte count", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		for n > 0 {
			chunk := min(n, int64(len(zeros)))
			if _, err := w.Write(zeros[:chunk]); err != nil {
				return
			}
			n -= chunk
		}
	})
	mux.HandleFunc("/feed/", serveFeed)
	addReleases(mux, releaseDir)
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintln(w, n)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Client-Addr", r.RemoteAddr)
		mux.ServeHTTP(w, r)
	})
}

// feedDir holds stand-in subscription feeds. <name> is the body, <name>.info its
// subscription-userinfo header, <name>.title its profile-title header, <name>.fail makes it
// answer 503, and <name>.ua makes it answer 404 to any User-Agent without that prefix, as a
// real provider does.
const feedDir = "/etc/unifi-lab/feeds"

func serveFeed(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Path)
	if _, err := os.Stat(filepath.Join(feedDir, name+".fail")); err == nil {
		http.Error(w, "feed down", http.StatusServiceUnavailable)
		return
	}
	if want, err := os.ReadFile(filepath.Join(feedDir, name+".ua")); err == nil && !strings.HasPrefix(r.UserAgent(), strings.TrimSpace(string(want))) {
		http.NotFound(w, r)
		return
	}
	body, err := os.ReadFile(filepath.Join(feedDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info, err := os.ReadFile(filepath.Join(feedDir, name+".info")); err == nil {
		w.Header().Set("Subscription-Userinfo", strings.TrimSpace(string(info)))
	}
	if title, err := os.ReadFile(filepath.Join(feedDir, name+".title")); err == nil {
		w.Header().Set("Profile-Title", strings.TrimSpace(string(title)))
	}
	w.Write(body)
}

// udpEchoAll binds each local IPv4 address separately so a reply leaves from
// the address the request was sent to.
func udpEchoAll(port string) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Fatal(err)
	}
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil {
			go udpEcho(net.JoinHostPort(ip.IP.String(), port))
		}
	}
}

func udpEcho(addr string) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 2048)
	for {
		_, from, err := conn.ReadFrom(buf)
		if err != nil {
			log.Fatal(err)
		}
		host, _, _ := net.SplitHostPort(from.String())
		conn.WriteTo([]byte(host+"\n"), from)
	}
}

func udpAsk(addr string) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	for range 3 {
		conn.Write([]byte("who\n"))
		n, err := conn.Read(buf)
		if err == nil {
			os.Stdout.Write(buf[:n])
			return
		}
	}
	fmt.Fprintln(os.Stderr, "no udp reply from", addr)
	os.Exit(1)
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "udp" {
		udpAsk(os.Args[2])
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "release-mode" {
		if err := setMode(releaseDir, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "labweb release-mode:", err)
			os.Exit(2)
		}
		return
	}
	h := handler()
	udpEchoAll("9000")
	udpEchoAll("443")
	go func() {
		log.Fatal(http.ListenAndServeTLS(":443", "/var/lib/unifi-lab/labweb.crt", "/var/lib/unifi-lab/labweb.pem", h))
	}()
	log.Fatal(http.ListenAndServe(":80", h))
}
