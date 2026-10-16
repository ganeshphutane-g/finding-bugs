package config

import (
	"net/url"
	"regexp"
	"strings"
)

// One rule decides what is a secret, for the export (written as ${NAME}),
// the diff (never printed) and the loaded file. When in doubt, it is a
// secret: a value hidden by mistake costs a line in an env file, a value
// shown by mistake can cost an account.

// monitorSecrets are monitor fields that hold credentials.
var monitorSecrets = map[string]bool{
	"basic_auth_pass": true, "oauth_client_secret": true, "bearer_token": true, "gamedigToken": true,
	"pushToken": true, "databaseConnectionString": true, "radiusPassword": true, "radiusSecret": true,
	"mqttPassword": true, "tlsKey": true, "kafkaProducerSaslOptions": true, "rabbitmqPassword": true,
	"sshPassword": true, "sshPrivateKey": true, "sshPassphrase": true,
}

// monitorText are monitor fields of free text (headers, request bodies)
// that are secret when they mention credentials.
var monitorText = map[string]bool{"headers": true, "body": true, "grpcBody": true, "grpcMetadata": true}

// credential matches words that mark a credential.
var credential = regexp.MustCompile(`(?i)(pass|token|secret|key|auth|bearer|cookie|session|credential|signature|sid)`)

// secretName matches setting names that hold credentials (notification
// settings vary by provider, so they are recognised by name).
var secretName = regexp.MustCompile(`(?i)(pass(word|phrase)?|secret|token|api_?key|key$|privatekey|private_key|webhook|access_?key|credential|signature|sendkey|botkey|auth_?key|authid|sid$|spt$|hashkey|clientkey|consumerkey|integrationkey|userkey)`)

// notSecretName are names that match secretName but hold settings, not
// credentials (webhookContentType, telegramUseTemplate).
var notSecretName = regexp.MustCompile(`(?i)(type|method|format|mode|usetemplate|template|priority|enabled|region|severity|version)$`)

// userinfo matches a URL with a password in it.
var userinfo = regexp.MustCompile(`://[^/@\s:]+:[^/@\s]+@`)

// secretSetting reports a notification setting that holds a credential:
// by its name, or because it is an address carrying more than a host (a
// webhook path, a token in the query, a password).
func secretSetting(key string, v any) bool {
	s, ok := v.(string)
	if !ok || s == "" {
		return false
	}
	if secretName.MatchString(key) && !notSecretName.MatchString(key) {
		return true
	}
	if credential.MatchString(key) && strings.Contains(strings.ToLower(key), "header") {
		return credential.MatchString(s)
	}
	return urlCarriesSecret(s, true)
}

// urlCarriesSecret reports an address with a password, a token-like query
// or (when pathIsSecret) any path, as webhook addresses have.
func urlCarriesSecret(s string, pathIsSecret bool) bool {
	if !strings.Contains(s, "://") {
		return false
	}
	if userinfo.MatchString(s) {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return true // unreadable: keep it hidden
	}
	for name := range u.Query() {
		if credential.MatchString(name) {
			return true
		}
	}
	if pathIsSecret {
		p := strings.Trim(u.EscapedPath(), "/")
		return p != "" || u.RawQuery != "" || (u.Opaque != "")
	}
	return false
}

// secretField reports a top-level field of an object that holds a
// credential.
func secretField(kind, field string, v any) bool {
	switch kind {
	case "monitors":
		if monitorSecrets[field] {
			return !isEmpty(v)
		}
		s, ok := v.(string)
		if !ok || s == "" {
			return false
		}
		if monitorText[field] {
			return credential.MatchString(s)
		}
		return urlCarriesSecret(s, false)
	case "proxies":
		return field == "password" && !isEmpty(v)
	case "remote_browsers":
		s, _ := v.(string)
		return field == "url" && (strings.Contains(strings.ToLower(s), "token=") || urlCarriesSecret(s, false))
	}
	return false
}
