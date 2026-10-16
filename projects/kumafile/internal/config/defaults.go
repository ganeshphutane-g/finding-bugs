package config

import (
	"encoding/json"
	"strconv"
)

// The baseline is how a monitor created in the web interface (Uptime Kuma
// 2.x) looks on the server before anything is changed. The file leaves out
// what matches it, and a field missing from the file means this value.
// Fields not listed here (new in a later Kuma) are always written, and are
// left alone when missing from the file.

func num(n int64) json.Number { return json.Number(strconv.FormatInt(n, 10)) }

var baseline = map[string]any{
	"accepted_statuscodes":                []any{"200-299"},
	"active":                              true,
	"authDomain":                          nil,
	"authMethod":                          nil,
	"authWorkstation":                     nil,
	"basic_auth_pass":                     "",
	"basic_auth_user":                     "",
	"bearer_token":                        "",
	"body":                                nil,
	"cacheBust":                           false,
	"conditions":                          []any{},
	"databaseConnectionString":            nil,
	"databaseQuery":                       nil,
	"description":                         nil,
	"dns_resolve_server":                  "",
	"dns_resolve_type":                    "A",
	"docker_container":                    "",
	"domainExpiryNotification":            true,
	"expectedTlsAlert":                    nil,
	"expectedValue":                       nil,
	"expiryNotification":                  false,
	"game":                                nil,
	"gamedigGivenPortOnly":                true,
	"gamedigToken":                        "",
	"grpcBody":                            nil,
	"grpcEnableTls":                       false,
	"grpcMetadata":                        nil,
	"grpcMethod":                          nil,
	"grpcProtobuf":                        nil,
	"grpcServiceName":                     nil,
	"grpcUrl":                             nil,
	"headers":                             nil,
	"hostname":                            nil,
	"httpBodyEncoding":                    "json",
	"ignoreTls":                           false,
	"interval":                            num(60),
	"invertKeyword":                       false,
	"ipFamily":                            nil,
	"jsonPath":                            "$",
	"jsonPathOperator":                    "==",
	"kafkaProducerAllowAutoTopicCreation": false,
	"kafkaProducerBrokers":                []any{},
	"kafkaProducerMessage":                nil,
	"kafkaProducerSaslOptions":            map[string]any{"mechanism": "None"},
	"kafkaProducerSsl":                    false,
	"kafkaProducerTopic":                  nil,
	"keyword":                             nil,
	"location":                            "world",
	"maxredirects":                        num(10),
	"maxretries":                          num(0),
	"method":                              "GET",
	"mqttCheckType":                       "keyword",
	"mqttPassword":                        "",
	"mqttSuccessMessage":                  "",
	"mqttTopic":                           "",
	"mqttUsername":                        "",
	"mqttWebsocketPath":                   "",
	"ntpRootDispersionThreshold":          num(500),
	"ntpStratumThreshold":                 num(5),
	"ntpTimeOffsetThreshold":              num(1000),
	"oauth_audience":                      nil,
	"oauth_auth_method":                   "client_secret_basic",
	"oauth_client_id":                     nil,
	"oauth_client_secret":                 nil,
	"oauth_scopes":                        nil,
	"oauth_token_url":                     nil,
	"packetSize":                          num(56),
	"ping_count":                          num(1),
	"ping_numeric":                        true,
	"ping_per_request_timeout":            num(2),
	"port":                                nil,
	"protocol":                            nil,
	"pushToken":                           nil,
	"rabbitmqNodes":                       []any{},
	"rabbitmqPassword":                    "",
	"rabbitmqUsername":                    "",
	"radiusCalledStationId":               nil,
	"radiusCallingStationId":              nil,
	"radiusPassword":                      nil,
	"radiusSecret":                        nil,
	"radiusUsername":                      nil,
	"resendInterval":                      num(0),
	"responseMaxLength":                   num(1024),
	"retryInterval":                       num(60),
	"retryOnlyOnStatusCodeFailure":        false,
	"saveErrorResponse":                   true,
	"saveResponse":                        false,
	"screenshot_delay":                    num(0),
	"sftpPath":                            nil,
	"smtpSecurity":                        nil,
	"snmpOid":                             nil,
	"snmpVersion":                         "2c",
	"sshAuthMethod":                       "password",
	"sshPassphrase":                       nil,
	"sshPassword":                         nil,
	"sshPrivateKey":                       nil,
	"sshUsername":                         nil,
	"subtype":                             nil,
	"system_service_name":                 "",
	"timeout":                             num(48),
	"tlsCa":                               nil,
	"tlsCert":                             nil,
	"tlsKey":                              nil,
	"upsideDown":                          false,
	"wsIgnoreSecWebsocketAcceptHeader":    false,
	"wsSubprotocol":                       ""}

// typeBaseline is what the web interface sets differently for some types.
var typeBaseline = map[string]map[string]any{
	"ping":              {"timeout": num(10)},
	"snmp":              {"timeout": num(5), "port": num(161)},
	"kafka-producer":    {"timeout": num(1)},
	"dns":               {"port": num(53), "dns_resolve_server": "1.1.1.1"},
	"radius":            {"port": num(1812)},
	"ntp":               {"port": num(123), "interval": num(300), "retryInterval": num(300)},
	"sftp":              {"port": num(22)},
	"globalping":        {"subtype": "ping"},
	"websocket-upgrade": {"accepted_statuscodes": []any{"1000"}},
}

// defaultFor is the baseline value of a monitor field for a type, and
// whether the field has one.
func defaultFor(typ, field string) (any, bool) {
	if v, ok := typeBaseline[typ][field]; ok {
		return v, true
	}
	v, ok := baseline[field]
	return v, ok
}

// kindBaseline is what a fresh object of the other kinds looks like.
var kindBaseline = map[string]map[string]any{
	"notifications": {"default": false},
	"proxies":       {"auth": false, "default": false},
	"maintenance":   {"active": true, "timezoneOption": "SAME_AS_SERVER", "description": "", "intervalDay": num(1)},
	"status_pages": {"icon": "/icon.svg", "theme": "auto", "autoRefreshInterval": num(300),
		"showTags": false, "showPoweredBy": true, "showCertificateExpiry": false, "showOnlyLastHeartbeat": false},
}

// managed are the fields kumafile looks after for each kind: compared, and
// set by apply. Monitors are managed field by field from the baseline,
// plus their references.
var managed = map[string][]string{
	"tags":            {"name", "color"},
	"notifications":   {"name", "type", "default", "settings"},
	"proxies":         {"protocol", "host", "port", "auth", "username", "password", "default"},
	"docker_hosts":    {"name", "dockerType", "dockerDaemon"},
	"remote_browsers": {"name", "url"},
	"maintenance": {"title", "description", "strategy", "active", "intervalDay", "dateRange", "timeRange", "weekdays",
		"daysOfMonth", "cron", "durationMinutes", "timezoneOption", "monitors", "status_pages"},
	"status_pages": {"slug", "title", "description", "icon", "theme", "showTags", "domainNameList",
		"customCSS", "footerText", "showPoweredBy", "analyticsId", "analyticsScriptUrl", "analyticsType",
		"showCertificateExpiry", "showOnlyLastHeartbeat", "rssTitle", "autoRefreshInterval", "groups", "logo"},
	"monitors": {"name", "group", "type", "notifications", "tags", "proxy", "docker_host", "remote_browser"},
}

// kindDefault is the baseline of a field of any kind.
func kindDefault(kind, typ, field string) (any, bool) {
	if kind == "monitors" {
		return defaultFor(typ, field)
	}
	v, ok := kindBaseline[kind][field]
	return v, ok
}

// placeholderURL are addresses the web interface fills in before one is
// typed; they mean no address.
var placeholderURL = map[string]bool{"https://": true, "http://": true, "wss://": true}

// maintenanceFields are the settings a maintenance strategy uses; the
// server keeps (or makes up) the others, and they are ignored.
func maintenanceFields(strategy string) map[string]bool {
	f := map[string]bool{"title": true, "description": true, "strategy": true, "active": true, "timezoneOption": true,
		"monitors": true, "status_pages": true}
	switch strategy {
	case "single":
		f["dateRange"] = true
	case "recurring-interval":
		f["dateRange"], f["timeRange"], f["intervalDay"] = true, true, true
	case "recurring-weekday":
		f["dateRange"], f["timeRange"], f["weekdays"] = true, true, true
	case "recurring-day-of-month":
		f["dateRange"], f["timeRange"], f["daysOfMonth"] = true, true, true
	case "cron":
		f["dateRange"], f["cron"], f["durationMinutes"] = true, true, true
	case "manual", "":
	default: // a strategy this version does not know: compare everything
		for _, k := range managed["maintenance"] {
			f[k] = true
		}
	}
	return f
}

// timeRange writes times as hours and minutes (the server adds seconds: 0).
func timeRange(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, len(list))
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			out[i] = e
			continue
		}
		c := map[string]any{}
		for k, x := range m {
			if k == "seconds" && same(x, 0) {
				continue
			}
			c[k] = x
		}
		out[i] = c
	}
	return out
}

// StatusPageFields are the status page settings apply sends.
func StatusPageFields() []string {
	var out []string
	for _, f := range managed["status_pages"] {
		if f != "groups" && f != "logo" {
			out = append(out, f)
		}
	}
	return out
}
