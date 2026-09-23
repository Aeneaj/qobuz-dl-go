package bundle

import (
	"testing"
)

// fakeBundleJS is a minimal snippet of bundle.js with all the patterns we extract.
// Seeds have no mid-string = padding (base64 of 24-byte strings = 32 chars with no =).
// Joined = 96 chars, trim 44 = 52 chars of valid base64.
const fakeBundleJS = `
var production:{api:{appId:"123456789",appSecret:"abcdef1234567890abcdef1234567890"}}
a.initialSeed("QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFB",window.utimezone.london)
a.initialSeed("RERERERERERERERERERERERERERERERE",window.utimezone.berlin)
a.initialSeed("R0dHR0dHR0dHR0dHR0dHR0dHR0dHR0dH",window.utimezone.abidjan)
name:"timezones/London",info:"QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJC",extras:"Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0ND"
name:"timezones/Berlin",info:"RUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVF",extras:"RkZGRkZGRkZGRkZGRkZGRkZGRkZGRkZG"
name:"timezones/Abidjan",info:"SEhISEhISEhISEhISEhISEhISEhISEhI",extras:"SUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJ"
privateKey: "mySecretKey123"
`

func TestBundleAppID(t *testing.T) {
	b := &Bundle{content: fakeBundleJS}
	id, err := b.AppID()
	if err != nil {
		t.Fatalf("AppID: %v", err)
	}
	if id != "123456789" {
		t.Errorf("AppID = %q, want %q", id, "123456789")
	}
}

func TestBundleAppID_NotFound(t *testing.T) {
	b := &Bundle{content: "no app id here"}
	_, err := b.AppID()
	if err == nil {
		t.Error("expected error when app_id not in bundle")
	}
}

func TestBundlePrivateKey(t *testing.T) {
	b := &Bundle{content: fakeBundleJS}
	key := b.PrivateKey()
	if key != "mySecretKey123" {
		t.Errorf("PrivateKey = %q, want %q", key, "mySecretKey123")
	}
}

func TestBundlePrivateKey_Missing(t *testing.T) {
	b := &Bundle{content: "no private key here"}
	key := b.PrivateKey()
	if key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
}

// Secrets come back in the Python original's order — bundle order with the
// second timezone moved to the front — and the same on every call. The
// fixture lists london, berlin, abidjan; each secret decodes to its seed's
// letter repeated (AAA…, DDD…, GGG…), which names the timezone it came from.
func TestBundleSecrets_Order(t *testing.T) {
	b := &Bundle{content: fakeBundleJS}
	for i := 0; i < 20; i++ {
		secrets, err := b.Secrets()
		if err != nil {
			t.Fatalf("Secrets: %v", err)
		}
		var got string
		for _, s := range secrets {
			got += s[:1]
		}
		if got != "DAG" { // berlin, london, abidjan
			t.Fatalf("call %d: secrets from %q, want berlin, london, abidjan (DAG)", i, got)
		}
	}
}

func TestBundleSecrets_NoSeeds(t *testing.T) {
	b := &Bundle{content: "no seeds here at all"}
	_, err := b.Secrets()
	if err == nil {
		t.Error("expected error when no seeds in bundle")
	}
}

func TestBundleRegexes(t *testing.T) {
	// Verify each regex individually
	tests := []struct {
		name    string
		content string
		found   bool
	}{
		{
			"app_id regex",
			`production:{api:{appId:"987654321",appSecret:"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"`,
			true,
		},
		{
			"app_id regex missing",
			`no app id`,
			false,
		},
		{
			"private key regex",
			`privateKey: "abc123XYZ"`,
			true,
		},
		{
			"bundle URL regex",
			`<script src="/resources/5.2.1-b042/bundle.js"></script>`,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var found bool
			switch {
			case tt.name == "app_id regex" || tt.name == "app_id regex missing":
				found = reAppID.MatchString(tt.content)
			case tt.name == "private key regex":
				for _, re := range rePrivateKeyPatterns {
					if re.MatchString(tt.content) {
						found = true
						break
					}
				}
			case tt.name == "bundle URL regex":
				found = reBundleURL.MatchString(tt.content)
			}
			if found != tt.found {
				t.Errorf("regex match = %v, want %v for content: %q", found, tt.found, tt.content)
			}
		})
	}
}
