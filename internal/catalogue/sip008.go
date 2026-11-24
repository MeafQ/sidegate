package catalogue

import (
	"encoding/json"
	"strings"

	"sidegate/internal/domain"
)

type sip008Server struct {
	ID         string     `json:"id"`
	Remarks    string     `json:"remarks"`
	Server     string     `json:"server"`
	ServerPort flexString `json:"server_port"`
	Password   string     `json:"password"`
	Method     string     `json:"method"`
	Plugin     string     `json:"plugin"`
}

// parseSIP008 keeps each server's id as the provider identity, the strongest
// lineage tier.
func (b *builder) parseSIP008(data []byte) error {
	var doc struct {
		Servers json.RawMessage `json:"servers"`
	}
	if json.Unmarshal(data, &doc) != nil || len(doc.Servers) == 0 || string(doc.Servers) == "null" {
		return ErrMalformed
	}
	servers, err := decodeList[json.RawMessage](b, doc.Servers)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		b.doc.explicitlyEmpty = true
	}
	for i, raw := range servers {
		var s sip008Server
		var d draft
		err := json.Unmarshal(raw, &s)
		if err != nil {
			err = reject(CodeMalformedEntry, "server")
		} else {
			d, err = sip008Draft(s)
		}
		if err := b.addServer(i+1, d, err); err != nil {
			return err
		}
	}
	return nil
}

func sip008Draft(s sip008Server) (draft, error) {
	if s.Plugin != "" {
		return draft{}, reject(CodeUnsupportedPlugin, "plugin")
	}
	if s.Server == "" {
		return draft{}, reject(CodeMalformedEntry, "server")
	}
	port, err := parsePort(string(s.ServerPort))
	if err != nil {
		return draft{}, err
	}
	cipher, err := shadowsocksCipher(s.Method)
	if err != nil {
		return draft{}, err
	}
	spec, err := newProtocol(domain.Shadowsocks, s.Password, "", cipher, "")
	if err != nil {
		return draft{}, err
	}
	d := single(s.Remarks, domain.Hop{
		Endpoint: domain.Endpoint{Host: s.Server, Port: port}, Protocol: spec, Transport: domain.Transport{Kind: domain.TCP},
	})
	d.providerID = strings.TrimSpace(s.ID)
	return d, nil
}
