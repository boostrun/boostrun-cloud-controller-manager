package provider

import (
	"io"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		file    string
		wantURL string
		wantErr string
	}{
		{name: "key only, default URL", env: map[string]string{EnvAPIKey: "k"}, wantURL: DefaultAPIURL},
		{name: "key file", env: map[string]string{EnvAPIKeyFile: "/run/secret/api-key"}, wantURL: DefaultAPIURL},
		{name: "env overrides the file", env: map[string]string{EnvAPIKey: "k", EnvAPIURL: "https://example.test/api/v1"},
			file: `{"apiURL":"https://other.test/api/v1"}`, wantURL: "https://example.test/api/v1"},
		{name: "file only", file: `{"apiURL":"http://fake:8080/api/v1","apiKey":"k"}`, wantURL: "http://fake:8080/api/v1"},
		{name: "no key", wantErr: "API key is not set"},
		{name: "key and key file", env: map[string]string{EnvAPIKey: "k", EnvAPIKeyFile: "/f"}, wantErr: "only one of"},
		{name: "not a URL", env: map[string]string{EnvAPIKey: "k", EnvAPIURL: "platform.boostrun.com"}, wantErr: "not an http(s) URL"},
		{name: "bad file", file: `{`, wantErr: "reading cloud config"},
		{name: "empty file", file: " ", env: map[string]string{EnvAPIKey: "k"}, wantURL: DefaultAPIURL},
		{name: "env key replaces the file's key file", env: map[string]string{EnvAPIKey: "k"},
			file: `{"apiKeyFile":"/f"}`, wantURL: DefaultAPIURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, env := range []string{EnvAPIURL, EnvAPIKey, EnvAPIKeyFile, EnvNetworkClusterID} {
				t.Setenv(env, tc.env[env])
			}
			var reader io.Reader
			if tc.file != "" {
				reader = strings.NewReader(tc.file)
			}
			config, err := loadConfig(reader)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || config.APIURL != tc.wantURL {
				t.Fatalf("config = %+v, err = %v", config, err)
			}
		})
	}
}
