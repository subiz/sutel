package sutel

import (
	"strings"
	"testing"
)

// RFC 2617 section 3.5 example: the canonical MD5 test vector.
func TestDigestAuthorizationMatchesRFC2617Vector(t *testing.T) {
	challenge := `Digest realm="testrealm@host.com", qop="auth,auth-int", ` +
		`nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`
	authorization, err := digestAuthorization(
		[]string{challenge}, "Mufasa", "Circle Of Life", "GET", "/dir/index.html", "0a4f113b",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`response="6629fae49393a05397450978507c4ef1"`,
		`username="Mufasa"`,
		`realm="testrealm@host.com"`,
		`nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093"`,
		`uri="/dir/index.html"`,
		`opaque="5ccc069c403ebaf9f0171e9517f40e41"`,
		"algorithm=MD5",
		"qop=auth",
		"nc=00000001",
		`cnonce="0a4f113b"`,
	} {
		if !strings.Contains(authorization, expected) {
			t.Fatalf("missing %s in:\n%s", expected, authorization)
		}
	}
}

// RFC 7616 section 3.9.1 example: the canonical SHA-256 test vector.
func TestDigestAuthorizationMatchesRFC7616SHA256Vector(t *testing.T) {
	challenge := `Digest realm="http-auth@example.org", qop="auth, auth-int", algorithm=SHA-256, ` +
		`nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`
	authorization, err := digestAuthorization(
		[]string{challenge}, "Mufasa", "Circle of Life", "GET", "/dir/index.html",
		"f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authorization, `response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"`) {
		t.Fatalf("wrong SHA-256 response:\n%s", authorization)
	}
	if !strings.Contains(authorization, "algorithm=SHA-256") {
		t.Fatalf("missing algorithm=SHA-256:\n%s", authorization)
	}
}

func TestDigestAuthorizationWithoutQOP(t *testing.T) {
	authorization, err := digestAuthorization(
		[]string{`Digest realm="testrealm@host.com", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093"`},
		"Mufasa", "Circle Of Life", "GET", "/dir/index.html", "unused-cnonce",
	)
	if err != nil {
		t.Fatal(err)
	}
	// MD5(HA1:nonce:HA2) per RFC 2069-compatible mode, no nc/cnonce/qop.
	if !strings.Contains(authorization, `response="670fd8c2df070c60b045671b8b24ff02"`) {
		t.Fatalf("wrong no-qop response:\n%s", authorization)
	}
	for _, forbidden := range []string{"qop=", "nc=", "cnonce="} {
		if strings.Contains(authorization, forbidden) {
			t.Fatalf("unexpected %s in no-qop authorization:\n%s", forbidden, authorization)
		}
	}
}

func TestDigestAuthorizationSkipsUnsupportedChallenges(t *testing.T) {
	authorization, err := digestAuthorization([]string{
		`Digest realm="r", nonce="n", algorithm=SHA-512-256`,
		`Basic realm="r"`,
		`Digest realm="r", nonce="n", algorithm=SHA-256`,
	}, "user", "pass", "INVITE", "sip:100@127.0.0.1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authorization, "algorithm=SHA-256") {
		t.Fatalf("did not select the SHA-256 challenge:\n%s", authorization)
	}
}

func TestDigestAuthorizationRejectsUnusableChallenges(t *testing.T) {
	for name, challenges := range map[string][]string{
		"empty":            {},
		"unsupported only": {`Digest realm="r", nonce="n", algorithm=SHA-512-256`},
		"basic only":       {`Basic realm="r"`},
		"missing nonce":    {`Digest realm="r"`},
		"missing realm":    {`Digest nonce="n"`},
		"qop mismatch":     {`Digest realm="r", nonce="n", qop="auth-int"`},
		"duplicate param":  {`Digest realm="r", realm="other", nonce="n"`},
		"malformed param":  {`Digest realm="r", nonce="n", broken`},
	} {
		if _, err := digestAuthorization(challenges, "u", "p", "INVITE", "sip:x", "cn"); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestDigestChallengeParsingHandlesQuotingAndEscapes(t *testing.T) {
	parameters, err := parseDigestChallenge(
		"  Digest realm=\"with, comma\", nonce=\"quo\\\"te\\\\slash\", algorithm=MD5",
	)
	if err != nil {
		t.Fatal(err)
	}
	if parameters["realm"] != "with, comma" || parameters["nonce"] != `quo"te\slash` {
		t.Fatalf("parameters=%+v", parameters)
	}
}
