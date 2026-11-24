// informsim plays an unadopted gateway to the lab's Network Application, over
// the inform protocol on the VM's loopback. It writes each decrypted reply to a
// directory, so the lab can read what the app would provision to a real device.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultKey = "ba86f2bbe107c7c57eb5f2690775c712"

const (
	flagEncrypted = 1 << iota
	flagZlib
	flagSnappy
	flagGCM
)

type device struct {
	mac       [6]byte
	macText   string
	model     string
	version   string
	informURL string
	key       []byte
	adopted   bool
	cfg       string
	out       string
	seq       int
}

func main() {
	informURL := flag.String("url", "http://127.0.0.1:8080/inform", "inform URL, loopback only")
	macText := flag.String("mac", "02:ab:cd:00:00:01", "device MAC")
	model := flag.String("model", "UDRULT", "device model")
	version := flag.String("version", "5.1.34.0", "firmware version")
	out := flag.String("out", "/tmp/informsim", "directory for decrypted replies")
	rounds := flag.Int("rounds", 40, "informs to send")
	every := flag.Duration("every", 3*time.Second, "time between informs")
	authkey := flag.String("authkey", "", "hex authkey of an already adopted device")
	flag.Parse()

	u, err := url.Parse(*informURL)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		log.Fatalf("inform URL %q is not loopback", *informURL)
	}
	d := &device{macText: *macText, model: *model, version: *version, informURL: *informURL, out: *out}
	raw, err := hex.DecodeString(strings.ReplaceAll(*macText, ":", ""))
	if err != nil || len(raw) != 6 {
		log.Fatalf("bad MAC %q", *macText)
	}
	copy(d.mac[:], raw)
	d.key, _ = hex.DecodeString(defaultKey)
	if *authkey != "" {
		if d.key, err = hex.DecodeString(*authkey); err != nil {
			log.Fatalf("bad authkey: %v", err)
		}
		d.adopted = true
	}
	if err := os.MkdirAll(d.out, 0o700); err != nil {
		log.Fatal(err)
	}
	for i := 0; i < *rounds; i++ {
		if err := d.inform(); err != nil {
			log.Printf("inform %d: %v", i, err)
		}
		time.Sleep(*every)
	}
}

func (d *device) payload() []byte {
	gw := map[string]any{
		"ip": "192.168.100.10", "netmask": "255.255.255.0", "name": "eth4", "mac": d.macText,
		"is_wan": true, "up": true, "speed": 1000, "full_duplex": true,
	}
	lan := map[string]any{
		"ip": "192.168.1.1", "netmask": "255.255.255.0", "name": "br0", "mac": d.macText,
		"up": true, "speed": 1000, "full_duplex": true,
	}
	m := map[string]any{
		"mac": d.macText, "model": d.model, "model_display": d.model, "serial": strings.ToUpper(strings.ReplaceAll(d.macText, ":", "")),
		"ip": "192.168.5.15", "netmask": "255.255.255.0", "hostname": "lab-gateway",
		"version": d.version, "required_version": "1.0.0", "bootrom_version": "unknown",
		"inform_url": d.informURL, "inform_ip": "127.0.0.1", "cfgversion": d.cfg,
		"default": !d.adopted, "discovery_response": false, "state": 0, "uptime": 1000 + d.seq,
		"time": time.Now().Unix(), "has_fan": false, "has_temperature": false, "board_rev": 1,
		"if_table":    []any{gw, lan},
		"sys_stats":   map[string]any{"loadavg_1": "0.1", "loadavg_5": "0.1", "loadavg_15": "0.1", "mem_total": 3 << 30, "mem_used": 1 << 30},
		"radio_table": []any{},
	}
	b, _ := json.Marshal(m)
	return b
}

func (d *device) inform() error {
	plain := d.payload()
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		return err
	}
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return err
	}
	pad := 16 - len(plain)%16
	padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	var msg bytes.Buffer
	msg.WriteString("TNBU")
	binary.Write(&msg, binary.BigEndian, int32(0))
	msg.Write(d.mac[:])
	binary.Write(&msg, binary.BigEndian, int16(flagEncrypted))
	msg.Write(iv)
	binary.Write(&msg, binary.BigEndian, int32(1))
	binary.Write(&msg, binary.BigEndian, int32(len(ct)))
	msg.Write(ct)

	req, err := http.NewRequest("POST", d.informURL, &msg)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-binary")
	req.Header.Set("User-Agent", "AirControl Agent v1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	reply, err := d.decode(body)
	if err != nil {
		return err
	}
	return d.apply(reply)
}

func (d *device) decode(b []byte) ([]byte, error) {
	if len(b) < 40 || string(b[:4]) != "TNBU" {
		return nil, errors.New("not an inform reply")
	}
	flags := binary.BigEndian.Uint16(b[14:16])
	iv := b[16:32]
	n := binary.BigEndian.Uint32(b[36:40])
	ct := b[40 : 40+n]
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return nil, err
	}
	var plain []byte
	if flags&flagGCM != 0 {
		gcm, err := cipher.NewGCMWithNonceSize(block, 16)
		if err != nil {
			return nil, err
		}
		plain, err = gcm.Open(nil, iv, ct, b[:40])
		if err != nil {
			return nil, fmt.Errorf("gcm: %w", err)
		}
	} else {
		plain = make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
		plain = plain[:len(plain)-int(plain[len(plain)-1])]
	}
	switch {
	case flags&flagSnappy != 0:
		return nil, errors.New("snappy reply: not handled")
	case flags&flagZlib != 0:
		r, err := zlib.NewReader(bytes.NewReader(plain))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(r)
	}
	return plain, nil
}

func (d *device) apply(reply []byte) error {
	var m map[string]any
	if err := json.Unmarshal(reply, &m); err != nil {
		return fmt.Errorf("reply is not JSON: %w", err)
	}
	kind, _ := m["_type"].(string)
	if kind != "noop" {
		d.seq++
		name := filepath.Join(d.out, strconv.Itoa(1000+d.seq)+"-"+kind+".json")
		if err := os.WriteFile(name, reply, 0o600); err != nil {
			return err
		}
	}
	log.Printf("reply %s keys=%d", kind, len(m))
	if mgmt, ok := m["mgmt_cfg"].(string); ok {
		for _, line := range strings.Split(mgmt, "\n") {
			k, v, _ := strings.Cut(line, "=")
			switch k {
			case "authkey":
				key, err := hex.DecodeString(v)
				if err != nil {
					return err
				}
				d.key, d.adopted = key, true
			case "cfgversion":
				d.cfg = v
			}
		}
	}
	return nil
}
