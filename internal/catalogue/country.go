package catalogue

import (
	"cmp"
	"errors"
	"regexp"
	"strings"

	"sidegate/internal/domain"
)

// CountryRule assigns Country to every server whose name matches Regex. Rules are the owner's and
// apply in order. The first match wins, ahead of any flag.
type CountryRule struct {
	Regex, Country string
}

type CountryPolicy struct {
	LabelRules []CountryRule
}

var ErrInvalidCountryRule = errors.New("catalogue: invalid country rule")

type countryRule struct {
	pattern *regexp.Regexp
	code    string
}

type countryResolver struct {
	rules []countryRule
}

func newCountryResolver(policy CountryPolicy) (*countryResolver, error) {
	r := &countryResolver{}
	for _, rule := range policy.LabelRules {
		code := strings.ToUpper(strings.TrimSpace(rule.Country))
		pattern, err := regexp.Compile(rule.Regex)
		if err != nil || len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return nil, ErrInvalidCountryRule
		}
		r.rules = append(r.rules, countryRule{pattern: pattern, code: code})
	}
	return r, nil
}

// country reads an owner's rule over the server's label, then the one flag its tags carry, then the
// one flag its home config's remarks carry.
func (r *countryResolver) country(n named) domain.Country {
	for _, rule := range r.rules {
		if rule.pattern.MatchString(n.label()) {
			return domain.Country{Code: rule.code, Source: domain.CountryRule}
		}
	}
	if code, ok := oneFlag(strings.Join(n.tags, " ")); ok {
		return domain.Country{Code: code, Source: domain.CountryTagFlag}
	}
	if code, ok := oneFlag(n.remarks); ok {
		return domain.Country{Code: code, Source: domain.CountryConfigFlag}
	}
	return domain.Country{}
}

const regionalA, regionalZ = 0x1f1e6, 0x1f1ff

// PlainLabel is a label without its flag emoji, which the page draws from the country instead. A label
// that is nothing but flags, as a config's remarks can be, stays as it is. A server's name never is.
func PlainLabel(label string) string { return cmp.Or(withoutFlags(label), label) }

func withoutFlags(label string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r >= regionalA && r <= regionalZ {
			return -1
		}
		return r
	}, label)), " ")
}

// oneFlag reads a label's flag emoji, each a pair of regional indicator symbols. Exactly
// one distinct flag names a country. Several or none name nothing.
func oneFlag(label string) (string, bool) {
	found := ""
	var first rune
	for _, r := range label {
		switch {
		case r < regionalA || r > regionalZ:
			first = 0
		case first == 0:
			first = r
		default:
			code := string([]rune{'A' + first - regionalA, 'A' + r - regionalA})
			first = 0
			if found != "" && found != code {
				return "", false
			}
			found = code
		}
	}
	return found, found != ""
}
