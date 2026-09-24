package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Systicore/systicore_go/logging"
)

// PlatformServer is the platform every Go service reports as.
const PlatformServer = "server"

// Field caps of the ingest contract. The backend truncates too, but
// capping here keeps a huge trace from pushing the whole body over the
// backend's body limit (413), which would drop the report.
const (
	maxTypeBytes        = 200
	maxCodeBytes        = 100
	maxMessageBytes     = 8 * 1024
	maxTraceBytes       = 32 * 1024
	maxActionBytes      = 200
	maxRouteBytes       = 500
	maxRequestIDBytes   = 100
	maxTags             = 10
	maxTagKeyBytes      = 32
	maxTagValueBytes    = 128
	maxFingerprintBytes = 64
	// maxDescriptorBytes caps the short strings the contract leaves
	// uncapped (user, release, environment, device), so none of them can
	// push the body over the limit on its own.
	maxDescriptorBytes = 200
	// maxBodyBytes is the backend's default INGEST_BODY_LIMIT (65536)
	// minus a margin. The field caps count raw bytes, while JSON escapes
	// control characters and invalid UTF-8 as six-byte sequences, so the
	// encoded body is checked against this limit as well.
	maxBodyBytes = 63 * 1024
)

// ingestPayload is the body of POST /api/v1/ingest (contract §3).
type ingestPayload struct {
	Error       errorPayload   `json:"error"`
	Release     Release        `json:"release,omitzero"`
	Environment string         `json:"environment,omitempty"`
	Platform    string         `json:"platform"`
	Device      Device         `json:"device"`
	User        userPayload    `json:"user,omitzero"`
	Context     contextPayload `json:"context,omitzero"`
}

type errorPayload struct {
	Type        string   `json:"type,omitempty"`
	Code        string   `json:"code,omitempty"`
	Message     string   `json:"message,omitempty"`
	Trace       string   `json:"trace,omitempty"`
	Action      string   `json:"action,omitempty"`
	Severity    Severity `json:"severity"`
	Fingerprint string   `json:"fingerprint,omitempty"`
}

type userPayload struct {
	ID     string `json:"id,omitempty"`
	Issuer string `json:"issuer,omitempty"`
}

type contextPayload struct {
	Route     string            `json:"route,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// prepare completes a report with what only the caller's ctx knows (the
// request id) before it is queued: the ctx does not travel with it.
func (c *Client) prepare(ctx context.Context, report Report) Report {
	if report.Context.RequestID == "" && ctx != nil {
		report.Context.RequestID = logging.RequestID(ctx)
	}
	return report
}

// encode renders the report as the ingest body: every field capped to
// the contract's size, and the encoded body kept under maxBodyBytes by
// cutting the trace, then the message, when escaping inflated it.
func (c *Client) encode(report Report) ([]byte, error) {
	payload := c.payloadOf(report)
	body, err := marshalPayload(payload)
	for err == nil && len(body) > maxBodyBytes && shrinkError(&payload.Error, len(body)-maxBodyBytes) {
		body, err = marshalPayload(payload)
	}
	return body, err
}

// payloadOf maps a report onto the ingest body with every field capped.
func (c *Client) payloadOf(report Report) ingestPayload {
	return ingestPayload{
		Error: errorPayload{
			Type:        truncateUTF8(report.Type, maxTypeBytes),
			Code:        truncateUTF8(report.Code, maxCodeBytes),
			Message:     truncateUTF8(report.Message, maxMessageBytes),
			Trace:       truncateUTF8(report.Trace, maxTraceBytes),
			Action:      truncateUTF8(report.Action, maxActionBytes),
			Severity:    normalizeSeverity(report.Severity),
			Fingerprint: sanitizeFingerprint(report.Fingerprint),
		},
		Release:     capRelease(c.options.Release),
		Environment: truncateUTF8(c.options.Environment, maxDescriptorBytes),
		Platform:    PlatformServer,
		Device:      capDevice(c.device),
		User: userPayload{
			ID:     truncateUTF8(report.User.ID, maxDescriptorBytes),
			Issuer: truncateUTF8(report.User.Issuer, maxDescriptorBytes),
		},
		Context: contextPayload{
			Route:     truncateUTF8(report.Context.Route, maxRouteBytes),
			RequestID: truncateUTF8(report.Context.RequestID, maxRequestIDBytes),
			Tags:      capTags(report.Context.Tags),
		},
	}
}

// marshalPayload encodes without HTML escaping: json.Marshal would turn
// every <, > and & (common in an upstream HTML error body) into a
// six-byte < sequence.
func marshalPayload(payload ingestPayload) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, fmt.Errorf("report: encode: %w", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// shrinkError frees excess encoded bytes from the trace, or from the
// message once the trace is empty, and reports whether anything was left
// to cut. Each cut shortens a field strictly, so encode's loop ends.
func shrinkError(errorObject *errorPayload, excess int) bool {
	switch {
	case errorObject.Trace != "":
		errorObject.Trace = truncateEncoded(errorObject.Trace, encodedLength(errorObject.Trace)-excess)
	case errorObject.Message != "":
		errorObject.Message = truncateEncoded(errorObject.Message, encodedLength(errorObject.Message)-excess)
	default:
		return false
	}
	return true
}

// truncateEncoded returns the longest prefix of value, cut on a rune
// boundary, whose JSON encoding takes at most budget bytes. Escapes make
// the encoded size uneven along the string, hence the binary search.
func truncateEncoded(value string, budget int) string {
	if budget <= 0 {
		return ""
	}
	fitting, tooLong := 0, len(value)+1
	for tooLong-fitting > 1 {
		middle := (fitting + tooLong) / 2
		if encodedLength(truncateUTF8(value, middle)) <= budget {
			fitting = middle
		} else {
			tooLong = middle
		}
	}
	return truncateUTF8(value, fitting)
}

// encodedLength is the size of value as a JSON string without its quotes,
// escaped the way marshalPayload escapes it.
func encodedLength(value string) int {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value) // a Go string always encodes
	return buffer.Len() - len("\"\"\n")
}

func capRelease(release Release) Release {
	return Release{
		Version:   truncateUTF8(release.Version, maxDescriptorBytes),
		Commit:    truncateUTF8(release.Commit, maxDescriptorBytes),
		BuildTime: truncateUTF8(release.BuildTime, maxDescriptorBytes),
	}
}

func capDevice(device Device) Device {
	return Device{
		Brand:      truncateUTF8(device.Brand, maxDescriptorBytes),
		Model:      truncateUTF8(device.Model, maxDescriptorBytes),
		OSVersion:  truncateUTF8(device.OSVersion, maxDescriptorBytes),
		APILevel:   device.APILevel,
		AppVersion: truncateUTF8(device.AppVersion, maxDescriptorBytes),
		InstallID:  truncateUTF8(device.InstallID, maxDescriptorBytes),
	}
}

// normalizeSeverity maps anything outside the enum to error: the backend
// answers an invalid severity with 400, which would lose the report.
func normalizeSeverity(severity Severity) Severity {
	switch severity {
	case SeverityWarning, SeverityError, SeverityCritical:
		return severity
	}
	return SeverityError
}

// sanitizeFingerprint keeps the override inside the contract's alphabet
// ([A-Za-z0-9._:-], at most 64 characters) by replacing anything else
// with '-', so a sloppy override still groups deterministically.
func sanitizeFingerprint(fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	var builder strings.Builder
	for _, character := range fingerprint {
		if builder.Len() == maxFingerprintBytes {
			break
		}
		if fingerprintCharacterAllowed(character) {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

func fingerprintCharacterAllowed(character rune) bool {
	switch {
	case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		return true
	case character == '.', character == '_', character == ':', character == '-':
		return true
	}
	return false
}

// capTags keeps at most maxTags tags with keys and values truncated. Keys
// are truncated before the choice, so two keys sharing their first
// maxTagKeyBytes bytes become one tag, not a silent overwrite: in sorted
// order the first original key keeps the truncated key, and the choice
// of which tags survive is deterministic.
func capTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	capped := make(map[string]string, min(len(tags), maxTags))
	for _, originalKey := range slices.Sorted(maps.Keys(tags)) {
		key := truncateUTF8(originalKey, maxTagKeyBytes)
		if _, taken := capped[key]; taken {
			continue
		}
		capped[key] = truncateUTF8(tags[originalKey], maxTagValueBytes)
		if len(capped) == maxTags {
			break
		}
	}
	return capped
}

// truncateUTF8 cuts value to at most maxBytes without splitting a rune.
func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
