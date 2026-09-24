package report

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Systicore/systicore_go/report/reporttest"
)

func TestPayloadIsCappedAndSanitized(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	tags := map[string]string{}
	for index := range 12 {
		tags[string(rune('a'+index))+strings.Repeat("k", 40)] = strings.Repeat("v", 200)
	}
	err := client.Submit(context.Background(), Report{
		Message:     strings.Repeat("é", maxMessageBytes), // 2 bytes per rune
		Trace:       strings.Repeat("x", 100*1024),
		Severity:    "fatal",
		Fingerprint: "orders sync/failed!" + strings.Repeat("z", 100),
		Context:     Context{Tags: tags},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := server.Requests()[0]
	if len(request.Raw) > maxBodyBytes {
		t.Errorf("body is %d bytes, must stay under the backend's 64 KB limit", len(request.Raw))
	}
	errorObject := request.Error()
	if message := errorObject["message"].(string); len(message) > maxMessageBytes || !strings.HasSuffix(message, "é") {
		t.Errorf("message must be cut on a rune boundary within %d bytes, got %d bytes", maxMessageBytes, len(message))
	}
	if trace := errorObject["trace"].(string); len(trace) != maxTraceBytes {
		t.Errorf("trace length = %d, want %d", len(trace), maxTraceBytes)
	}
	if errorObject["severity"] != "error" {
		t.Errorf("an unknown severity must fall back to error, got %v", errorObject["severity"])
	}
	fingerprint := errorObject["fingerprint"].(string)
	if len(fingerprint) != maxFingerprintBytes || !strings.HasPrefix(fingerprint, "orders-sync-failed-") {
		t.Errorf("fingerprint = %q", fingerprint)
	}
	sentTags := request.Context()["tags"].(map[string]any)
	if len(sentTags) != maxTags {
		t.Errorf("tags = %d, want %d", len(sentTags), maxTags)
	}
	for key, value := range sentTags {
		if len(key) > maxTagKeyBytes || len(value.(string)) > maxTagValueBytes {
			t.Errorf("tag %q=%q exceeds the caps", key, value)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	cases := []struct {
		value    string
		maxBytes int
		want     string
	}{
		{"short", 10, "short"},
		{"abcdef", 3, "abc"},
		{"aé", 2, "a"}, // é is two bytes and must not be split
		{"日本", 4, "日"},
	}
	for _, testCase := range cases {
		if got := truncateUTF8(testCase.value, testCase.maxBytes); got != testCase.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", testCase.value, testCase.maxBytes, got, testCase.want)
		}
	}
}

func TestBodyIsNotHTMLEscaped(t *testing.T) {
	// json.Marshal would send each of <, > and & as a six-byte <
	// escape, so an upstream HTML body could push the report over the limit.
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	message := strings.Repeat("<", maxMessageBytes)
	trace := strings.Repeat("&>", maxTraceBytes/2)
	if err := client.Submit(context.Background(), Report{Message: message, Trace: trace}); err != nil {
		t.Fatal(err)
	}
	request := server.Requests()[0]
	if len(request.Raw) > maxBodyBytes {
		t.Errorf("body is %d bytes, want at most %d", len(request.Raw), maxBodyBytes)
	}
	if !bytes.Contains(request.Raw, []byte("<<<<")) || !bytes.Contains(request.Raw, []byte("&>&>")) {
		t.Error("<, > and & must be sent as they are")
	}
	if errorObject := request.Error(); errorObject["message"] != message || errorObject["trace"] != trace {
		t.Error("a body under the limit must keep the message and the trace whole")
	}
}

func TestBodyStaysUnderTheLimitWhenEscapingInflatesIt(t *testing.T) {
	// Control characters, invalid UTF-8 and U+2028 are escaped as six
	// bytes each: a trace within its 32 KB cap can encode to 192 KB.
	cases := map[string]string{
		"control characters": strings.Repeat("\x01", maxTraceBytes),
		"invalid UTF-8":      strings.Repeat("\xff", maxTraceBytes),
		"line separators":    strings.Repeat(" ", maxTraceBytes/3),
	}
	for name, trace := range cases {
		t.Run(name, func(t *testing.T) {
			server := reporttest.NewServer(t)
			client, _ := newTestClient(t, server)
			message := strings.Repeat("\x02", maxMessageBytes)
			if err := client.Submit(context.Background(), Report{Code: "job.failed", Message: message, Trace: trace}); err != nil {
				t.Fatal(err)
			}
			request := server.Requests()[0]
			// The longest escape is six bytes, so a tight cut lands within
			// six bytes of the limit.
			if length := len(request.Raw); length > maxBodyBytes || length <= maxBodyBytes-6 {
				t.Errorf("body is %d bytes, want the trace cut to just under %d", length, maxBodyBytes)
			}
			errorObject := request.Error()
			if errorObject["message"] != message {
				t.Error("the trace must be cut before the message")
			}
			if sentTrace, _ := errorObject["trace"].(string); sentTrace == "" {
				t.Error("the trace must be cut to fit, not dropped")
			}
		})
	}
}

func TestShrinkErrorCutsTheTraceBeforeTheMessage(t *testing.T) {
	errorObject := errorPayload{Message: "message", Trace: "trace"}
	steps := []struct {
		excess                    int
		wantMessage, wantTrace    string
		wantSomethingLeftToShrink bool
	}{
		{3, "message", "tr", true},
		{10, "message", "", true},
		{3, "mess", "", true},
		{100, "", "", true},
		{1, "", "", false},
	}
	for _, step := range steps {
		if shrunk := shrinkError(&errorObject, step.excess); shrunk != step.wantSomethingLeftToShrink {
			t.Errorf("shrinkError(%d) = %v, want %v", step.excess, shrunk, step.wantSomethingLeftToShrink)
		}
		if errorObject.Message != step.wantMessage || errorObject.Trace != step.wantTrace {
			t.Errorf("after cutting %d: message=%q trace=%q, want %q / %q",
				step.excess, errorObject.Message, errorObject.Trace, step.wantMessage, step.wantTrace)
		}
	}
}

func TestTruncateEncoded(t *testing.T) {
	cases := []struct {
		value  string
		budget int
		want   string
	}{
		{"plain", 10, "plain"},
		{"plain", 3, "pla"},
		{"a\x01b", 7, "a\x01"}, // \x01 is sent as the six bytes \u0001
		{"a\x01b", 6, "a"},
		{"a\"b", 2, "a"}, // a quote is sent as \"
		{"<é>", 3, "<é"}, // no HTML escaping; é is two bytes and never split
		{"<é>", 2, "<"},
		{"anything", 0, ""},
	}
	for _, testCase := range cases {
		if got := truncateEncoded(testCase.value, testCase.budget); got != testCase.want {
			t.Errorf("truncateEncoded(%q, %d) = %q, want %q", testCase.value, testCase.budget, got, testCase.want)
		}
	}
}

func TestFieldsTheContractLeavesUncappedAreCappedToo(t *testing.T) {
	long := strings.Repeat("u", 100*1024)
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server, func(options *Options) {
		options.Release = Release{Version: long, Commit: long, BuildTime: long}
		options.Environment = long
		options.InstanceID = long
	})
	if err := client.Submit(context.Background(), Report{Message: "x", User: User{ID: long, Issuer: long}}); err != nil {
		t.Fatal(err)
	}
	request := server.Requests()[0]
	if len(request.Raw) > maxBodyBytes {
		t.Errorf("body is %d bytes, want at most %d", len(request.Raw), maxBodyBytes)
	}
	release := request.Body["release"].(map[string]any)
	for name, value := range map[string]any{
		"user.id":           request.User()["id"],
		"user.issuer":       request.User()["issuer"],
		"release.version":   release["version"],
		"release.commit":    release["commit"],
		"release.buildTime": release["buildTime"],
		"environment":       request.Body["environment"],
		"device.appVersion": request.Device()["appVersion"],
		"device.installId":  request.Device()["installId"],
	} {
		if text, _ := value.(string); len(text) != maxDescriptorBytes {
			t.Errorf("%s is %d bytes, want %d", name, len(text), maxDescriptorBytes)
		}
	}
}

func TestCapTagsTruncatesKeysBeforeChoosingThem(t *testing.T) {
	sharedPrefix := strings.Repeat("p", maxTagKeyBytes)
	tags := map[string]string{
		sharedPrefix + "-first":  "kept",
		sharedPrefix + "-second": "collides with the first once truncated",
	}
	for index := range maxTags {
		tags[fmt.Sprintf("tag%02d", index)] = "value"
	}
	// Map iteration order varies between runs; the result must not.
	for range 20 {
		capped := capTags(tags)
		if len(capped) != maxTags {
			t.Fatalf("tags = %d, want %d distinct keys: %v", len(capped), maxTags, capped)
		}
		if capped[sharedPrefix] != "kept" {
			t.Fatalf("the first key in sorted order must keep the truncated key, got %q", capped[sharedPrefix])
		}
		if _, found := capped["tag08"]; !found {
			t.Fatalf("the colliding key must not use up a slot: %v", capped)
		}
	}
}
