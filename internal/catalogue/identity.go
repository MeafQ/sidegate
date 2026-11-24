package catalogue

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"maps"
	"slices"
	"strings"

	"sidegate/internal/domain"
)

// identify derives every match key of a server. Hop IDs are presentation tags, so
// the closure is walked in dial order and no key ever includes an ID.
//
// A Reality server accepts a list of server names and a list of short IDs, and a
// panel may hand each client any member of each list: 3x-ui picks both at random
// on every subscription request. On a Reality hop the server name and short ID
// are therefore picks, not identity. No key includes them, so a refresh that only
// re-picks keeps every lineage, and the stored server keeps the pick it has. That
// pick may stop working when the provider rotates its lists, so a stored pick no
// refresh has carried for a day, over at least three refreshes, is replaced by
// the latest one as a new revision (update in lineage.go). The public key, spider
// path, and fingerprint stay in the keys. On a TLS hop the server name is the
// certificate the client verifies, and it stays in every key.
func identify(server domain.Server, providerID string) domain.Identity {
	_, hops := server.Closure()
	id := domain.Identity{
		ProviderID: providerID,
		Strong:     closureDigest(hops, true),
		Endpoint:   closureDigest(hops, false),
	}
	root := hops[0]
	protocol, _, _, _, _ := root.Protocol.Fields()
	host := sha256.New()
	writeFields(host, "host", strings.ToLower(root.Endpoint.Host))
	writeUint(host, uint64(protocol), uint64(root.Endpoint.Port), uint64(root.Transport.Kind))
	writeFields(host, strings.ToLower(verifiedName(root.TLS)))
	host.Sum(id.HostKey[:0])
	if r := root.TLS.Reality; r != nil {
		tie := sha256.New()
		writeFields(tie, "reality", r.PublicKey)
		tie.Sum(id.RealityTie[:0])
	}
	if _, secret, user, _, _ := root.Protocol.Fields(); !secret.IsZero() || user != "" {
		credentials := sha256.New()
		writeFields(credentials, "credentials", secret.Reveal(), user)
		credentials.Sum(id.CredentialKey[:0])
	}
	return id
}

// nameKey digests a base label the provider gave, trimmed and case folded. An exact
// comparison on purpose: a label that embeds traffic counters changes every refresh and
// simply doesn't match, where a normalized one could join two different servers.
func nameKey(label string) domain.Digest {
	var d domain.Digest
	if folded := strings.ToLower(strings.TrimSpace(label)); folded != "" {
		h := sha256.New()
		writeFields(h, "name", folded)
		h.Sum(d[:0])
	}
	return d
}

func closureDigest(hops []domain.Hop, credentials bool) domain.Digest {
	h := sha256.New()
	for _, hop := range hops {
		p, secret, user, cipher, flow := hop.Protocol.Fields()
		encryption := hop.Protocol.VLESSEncryption()
		writeUint(h, uint64(p), uint64(hop.Endpoint.Port), uint64(hop.Transport.Kind))
		writeFields(h, strings.ToLower(hop.Endpoint.Host), cipher, flow)
		t := hop.Transport
		if credentials {
			writeFields(h, secret.Reveal(), user, t.Salamander.Reveal())
		}
		writeUint(h, boolBit(!t.Salamander.IsZero()))
		writeFields(h, t.Path, t.Host, t.Service, t.Mode)
		for _, k := range slices.Sorted(maps.Keys(t.Headers)) {
			writeFields(h, k, t.Headers[k])
		}
		tls := hop.TLS
		writeUint(h, boolBit(tls.Enabled), uint64(len(tls.ALPN)), uint64(len(tls.PinnedCertSHA256)), uint64(len(tls.VerifyNames)))
		writeFields(h, verifiedName(tls), tls.Fingerprint)
		writeFields(h, tls.ALPN...)
		writeFields(h, tls.PinnedCertSHA256...)
		writeFields(h, tls.VerifyNames...)
		if r := tls.Reality; r != nil {
			writeFields(h, "reality", r.PublicKey, r.SpiderX)
		}
		// Each of these writes nothing when unset, so a server stored before it existed keeps its digest.
		if r := tls.Reality; r != nil && r.MLDSA65Verify != "" {
			writeFields(h, "mldsa65", r.MLDSA65Verify)
		}
		if tls.ECHConfigList != "" {
			writeRotating(h, "ech", tls.ECHConfigList, credentials)
		}
		if !encryption.IsZero() {
			writeRotating(h, "encryption", encryption.Reveal(), credentials)
		}
		if len(t.TCPMasks) > 0 {
			writeFields(h, "tcpmasks")
			writeUint(h, uint64(len(t.TCPMasks)))
			for _, m := range t.TCPMasks {
				writeTCPMask(h, m)
			}
		}
		if q := t.QuicParams; q != nil {
			writeFields(h, "quic", q.Congestion, q.BBRProfile, q.BrutalUp, q.BrutalDown)
			writeUint(h, boolBit(q.BrutalDisableLossCompensation), q.InitStreamReceiveWindow, q.MaxStreamReceiveWindow,
				q.InitConnectionReceiveWindow, q.MaxConnectionReceiveWindow, uint64(q.MaxIdleTimeout), uint64(q.KeepAlivePeriod),
				boolBit(q.DisablePathMTUDiscovery), boolBit(q.DisableChromeParrot), boolBit(q.DisableGSO),
				boolBit(q.DisableStatelessReset), uint64(q.MaxIncomingStreams))
		}
		writeFields(h, "|")
	}
	var d domain.Digest
	h.Sum(d[:0])
	return d
}

// writeRotating writes a key the provider replaces on its own, as it does a credential: the value is in
// the strong digest alone, and the endpoint's only says one is there.
func writeRotating(h hash.Hash, name, value string, credentials bool) {
	writeFields(h, name)
	if credentials {
		writeFields(h, value)
	}
}

func writeTCPMask(h hash.Hash, m domain.TCPMask) {
	writeFields(h, m.Type)
	if f := m.Fragment; f != nil {
		writeFields(h, f.Packets, f.Length, f.Delay, f.MaxSplit)
		for _, list := range [][]string{f.Lengths, f.Delays} {
			writeUint(h, uint64(len(list)))
			writeFields(h, list...)
		}
	}
	if c := m.HeaderCustom; c != nil {
		for _, sequences := range [][][]domain.CustomTCPItem{c.Clients, c.Servers, c.Errors} {
			writeUint(h, uint64(len(sequences)))
			for _, items := range sequences {
				writeUint(h, uint64(len(items)))
				for _, i := range items {
					writeUint(h, uint64(i.Rand))
					writeFields(h, i.Delay, i.RandRange, i.Capture, i.Reuse, string(i.Packet))
				}
			}
		}
	}
}

// revisionDigest names one stored server: its strong identity and the Reality
// picks it carries, so adopting a new pick is a new revision.
func revisionDigest(strong domain.Digest, server domain.Server) domain.Digest {
	h := sha256.New()
	h.Write(strong[:])
	picks := realityPicks(server)
	writeUint(h, uint64(len(picks)))
	for _, p := range picks {
		writeFields(h, p.serverName, p.shortID)
	}
	var d domain.Digest
	h.Sum(d[:0])
	return d
}

// verifiedName is the server name that identifies a hop: the TLS one, never a
// Reality pick.
func verifiedName(tls domain.TLS) string {
	if tls.Reality != nil {
		return ""
	}
	return tls.ServerName
}

func writeFields(h hash.Hash, fields ...string) {
	for _, f := range fields {
		writeUint(h, uint64(len(f)))
		h.Write([]byte(f))
	}
}

func writeUint(h hash.Hash, values ...uint64) {
	var b [8]byte
	for _, v := range values {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
}

func boolBit(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}
