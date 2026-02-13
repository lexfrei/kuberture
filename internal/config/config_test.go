package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	return path
}

func TestLoad_ValidFullConfig(t *testing.T) {
	yaml := `
source:
  namespace: kube-system
  serviceName: kube-apiserver
metricsBindAddress: ":9090"
healthProbeBindAddress: ":9091"
outputs:
  - name: prod
    hostname:
      - example.com
      - www.example.com
    annotationPrefix: "custom.io/"
    serviceName: my-svc
    recordTTL: 300
    addressSource: node-external
    addressType: IPv6
`
	cfg, err := Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Source.Namespace != "kube-system" {
		t.Errorf("source namespace = %q, want kube-system", cfg.Source.Namespace)
	}

	if cfg.Source.ServiceName != "kube-apiserver" {
		t.Errorf("source serviceName = %q, want kube-apiserver", cfg.Source.ServiceName)
	}

	if cfg.MetricsBindAddress != ":9090" {
		t.Errorf("metricsBindAddress = %q, want :9090", cfg.MetricsBindAddress)
	}

	if cfg.HealthProbeBindAddress != ":9091" {
		t.Errorf("healthProbeBindAddress = %q, want :9091", cfg.HealthProbeBindAddress)
	}

	if len(cfg.Outputs) != 1 {
		t.Fatalf("outputs count = %d, want 1", len(cfg.Outputs))
	}

	out := cfg.Outputs[0]
	if out.Name != "prod" {
		t.Errorf("output name = %q, want prod", out.Name)
	}

	if len(out.Hostnames) != 2 {
		t.Errorf("hostnames count = %d, want 2", len(out.Hostnames))
	}

	if out.AnnotationPrefix != "custom.io/" {
		t.Errorf("annotationPrefix = %q, want custom.io/", out.AnnotationPrefix)
	}

	if out.RecordTTL != 300 {
		t.Errorf("recordTTL = %d, want 300", out.RecordTTL)
	}

	if out.AddressSource != "node-external" {
		t.Errorf("addressSource = %q, want node-external", out.AddressSource)
	}

	if out.AddressType != AddressTypeIPv6 {
		t.Errorf("addressType = %q, want IPv6", out.AddressType)
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	yaml := `
outputs:
  - name: minimal
    hostname:
      - test.example.com
    serviceName: test-svc
`
	cfg, err := Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Source.Namespace != defaultNamespace {
		t.Errorf("source namespace = %q, want %s", cfg.Source.Namespace, defaultNamespace)
	}

	if cfg.Source.ServiceName != defaultServiceName {
		t.Errorf("source serviceName = %q, want kubernetes", cfg.Source.ServiceName)
	}

	if cfg.MetricsBindAddress != ":8080" {
		t.Errorf("metricsBindAddress = %q, want :8080", cfg.MetricsBindAddress)
	}

	if cfg.HealthProbeBindAddress != ":8081" {
		t.Errorf("healthProbeBindAddress = %q, want :8081", cfg.HealthProbeBindAddress)
	}

	out := cfg.Outputs[0]
	if out.AnnotationPrefix != "external-dns.alpha.kubernetes.io/" {
		t.Errorf("annotationPrefix = %q, want external-dns.alpha.kubernetes.io/", out.AnnotationPrefix)
	}

	if out.RecordTTL != 60 {
		t.Errorf("recordTTL = %d, want 60", out.RecordTTL)
	}

	if out.AddressSource != "endpointslice" {
		t.Errorf("addressSource = %q, want endpointslice", out.AddressSource)
	}

	if out.AddressType != "IPv4" {
		t.Errorf("addressType = %q, want IPv4", out.AddressType)
	}
}

func TestLoad_Validation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing outputs",
			yaml:    `source: {namespace: default}`,
			wantErr: "at least one output is required",
		},
		{
			name: "invalid address source",
			yaml: `
outputs:
  - name: bad-source
    hostname: [test.com]
    serviceName: svc
    addressSource: magic
`,
			wantErr: "invalid addressSource",
		},
		{
			name: "invalid address type",
			yaml: `
outputs:
  - name: bad-type
    hostname: [test.com]
    serviceName: svc
    addressType: IPv8
`,
			wantErr: "invalid addressType",
		},
		{
			name: "annotation prefix without trailing slash",
			yaml: `
outputs:
  - name: bad-prefix
    hostname: [test.com]
    serviceName: svc
    annotationPrefix: "no-slash"
`,
			wantErr: "annotationPrefix must end with /",
		},
		{
			name: "duplicate output names",
			yaml: `
outputs:
  - name: dup
    hostname: [a.com]
    serviceName: svc-a
  - name: dup
    hostname: [b.com]
    serviceName: svc-b
`,
			wantErr: "duplicate output name",
		},
		{
			name: "duplicate service name",
			yaml: `
outputs:
  - name: first
    hostname: [a.com]
    serviceName: shared-svc
  - name: second
    hostname: [b.com]
    serviceName: shared-svc
`,
			wantErr: "duplicate serviceName",
		},
		{
			name: "missing hostname",
			yaml: `
outputs:
  - name: no-host
    serviceName: svc
`,
			wantErr: "at least one hostname is required",
		},
		{
			name: "missing service name",
			yaml: `
outputs:
  - name: no-svc
    hostname: [test.com]
`,
			wantErr: "serviceName is required",
		},
		{
			name: "missing output name",
			yaml: `
outputs:
  - hostname: [test.com]
    serviceName: svc
`,
			wantErr: "output name is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTestConfig(t, tc.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}

	if !strings.Contains(err.Error(), "reading config file") {
		t.Errorf("error = %q, want substring %q", err.Error(), "reading config file")
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	_, err := Load(writeTestConfig(t, ""))
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}

	if !strings.Contains(err.Error(), "at least one output is required") {
		t.Errorf("error = %q, want substring %q", err.Error(), "at least one output is required")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	_, err := Load(writeTestConfig(t, "{{{{not: valid: yaml::::"))
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}

	if !strings.Contains(err.Error(), "parsing config YAML") {
		t.Errorf("error = %q, want substring %q", err.Error(), "parsing config YAML")
	}
}

func TestLoad_NegativeRecordTTL(t *testing.T) {
	cfgYAML := `
outputs:
  - name: neg-ttl
    hostname: [test.com]
    serviceName: svc
    recordTTL: -1
`

	_, err := Load(writeTestConfig(t, cfgYAML))
	if err == nil {
		t.Fatal("expected error for negative recordTTL, got nil")
	}

	if !strings.Contains(err.Error(), "recordTTL must be between 1 and") {
		t.Errorf("error = %q, want substring %q", err.Error(), "recordTTL must be between 1 and")
	}
}

func TestLoad_ExcessiveRecordTTL(t *testing.T) {
	cfgYAML := `
outputs:
  - name: big-ttl
    hostname: [test.com]
    serviceName: svc
    recordTTL: 100000
`

	_, err := Load(writeTestConfig(t, cfgYAML))
	if err == nil {
		t.Fatal("expected error for excessive recordTTL, got nil")
	}

	if !strings.Contains(err.Error(), "recordTTL must be between 1 and") {
		t.Errorf("error = %q, want substring %q", err.Error(), "recordTTL must be between 1 and")
	}
}

func TestLoad_MaxValidRecordTTL(t *testing.T) {
	cfgYAML := `
outputs:
  - name: max-ttl
    hostname: [test.com]
    serviceName: svc
    recordTTL: 86400
`

	cfg, err := Load(writeTestConfig(t, cfgYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Outputs[0].RecordTTL != 86400 {
		t.Errorf("recordTTL = %d, want 86400", cfg.Outputs[0].RecordTTL)
	}
}

func TestLoad_ZeroRecordTTLGetsDefault(t *testing.T) {
	cfgYAML := `
outputs:
  - name: default-ttl
    hostname: [test.com]
    serviceName: svc
`

	cfg, err := Load(writeTestConfig(t, cfgYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Outputs[0].RecordTTL != 60 {
		t.Errorf("recordTTL = %d, want 60 (default)", cfg.Outputs[0].RecordTTL)
	}
}

func TestLoad_MultipleOutputsAllValid(t *testing.T) {
	cfgYAML := `
outputs:
  - name: first
    hostname: [a.example.com]
    serviceName: svc-a
  - name: second
    hostname: [b.example.com]
    serviceName: svc-b
  - name: third
    hostname: [c.example.com, d.example.com]
    serviceName: svc-c
    recordTTL: 120
    addressSource: node-internal
    addressType: IPv6
`

	cfg, err := Load(writeTestConfig(t, cfgYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Outputs) != 3 {
		t.Fatalf("outputs count = %d, want 3", len(cfg.Outputs))
	}

	if cfg.Outputs[0].Name != "first" {
		t.Errorf("outputs[0].Name = %q, want first", cfg.Outputs[0].Name)
	}

	if cfg.Outputs[1].Name != "second" {
		t.Errorf("outputs[1].Name = %q, want second", cfg.Outputs[1].Name)
	}

	if cfg.Outputs[2].Name != "third" {
		t.Errorf("outputs[2].Name = %q, want third", cfg.Outputs[2].Name)
	}

	if len(cfg.Outputs[2].Hostnames) != 2 {
		t.Errorf("outputs[2] hostnames count = %d, want 2", len(cfg.Outputs[2].Hostnames))
	}

	if cfg.Outputs[2].RecordTTL != 120 {
		t.Errorf("outputs[2].RecordTTL = %d, want 120", cfg.Outputs[2].RecordTTL)
	}

	if cfg.Outputs[2].AddressSource != "node-internal" {
		t.Errorf("outputs[2].AddressSource = %q, want node-internal", cfg.Outputs[2].AddressSource)
	}

	if cfg.Outputs[2].AddressType != "IPv6" {
		t.Errorf("outputs[2].AddressType = %q, want IPv6", cfg.Outputs[2].AddressType)
	}
}

func TestLoad_AllAddressSources(t *testing.T) {
	sources := []string{"endpointslice", "node-internal", "node-external", "node-public"}

	for _, src := range sources {
		t.Run(src, func(t *testing.T) {
			cfgYAML := `
outputs:
  - name: test
    hostname: [test.com]
    serviceName: svc
    addressSource: ` + src + `
`

			cfg, err := Load(writeTestConfig(t, cfgYAML))
			if err != nil {
				t.Fatalf("unexpected error for addressSource %q: %v", src, err)
			}

			if cfg.Outputs[0].AddressSource != src {
				t.Errorf("addressSource = %q, want %q", cfg.Outputs[0].AddressSource, src)
			}
		})
	}
}

func TestLoad_AllAddressTypes(t *testing.T) {
	types := []string{"IPv4", "IPv6"}

	for _, addrType := range types {
		t.Run(addrType, func(t *testing.T) {
			cfgYAML := `
outputs:
  - name: test
    hostname: [test.com]
    serviceName: svc
    addressType: ` + addrType + `
`

			cfg, err := Load(writeTestConfig(t, cfgYAML))
			if err != nil {
				t.Fatalf("unexpected error for addressType %q: %v", addrType, err)
			}

			if cfg.Outputs[0].AddressType != addrType {
				t.Errorf("addressType = %q, want %q", cfg.Outputs[0].AddressType, addrType)
			}
		})
	}
}

func TestLoad_SourceDefaults(t *testing.T) {
	cfgYAML := `
outputs:
  - name: test
    hostname: [test.com]
    serviceName: svc
`

	cfg, err := Load(writeTestConfig(t, cfgYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Source.Namespace != defaultNamespace {
		t.Errorf("source.Namespace = %q, want %q", cfg.Source.Namespace, defaultNamespace)
	}

	if cfg.Source.ServiceName != defaultServiceName {
		t.Errorf("source.ServiceName = %q, want %q", cfg.Source.ServiceName, defaultServiceName)
	}
}

func TestLoad_EmptyHostnamesList(t *testing.T) {
	cfgYAML := `
outputs:
  - name: empty-hosts
    hostname: []
    serviceName: svc
`

	_, err := Load(writeTestConfig(t, cfgYAML))
	if err == nil {
		t.Fatal("expected error for empty hostnames list, got nil")
	}

	if !strings.Contains(err.Error(), "at least one hostname is required") {
		t.Errorf("error = %q, want substring %q", err.Error(), "at least one hostname is required")
	}
}

func TestLoad_AnnotationPrefixWithoutTrailingSlashVariants(t *testing.T) {
	prefixes := []string{"no-slash", "prefix.", "test:"}

	for _, prefix := range prefixes {
		t.Run(prefix, func(t *testing.T) {
			cfgYAML := `
outputs:
  - name: bad-prefix
    hostname: [test.com]
    serviceName: svc
    annotationPrefix: "` + prefix + `"
`

			_, err := Load(writeTestConfig(t, cfgYAML))
			if err == nil {
				t.Fatalf("expected error for annotationPrefix %q, got nil", prefix)
			}

			if !strings.Contains(err.Error(), "annotationPrefix must end with /") {
				t.Errorf("error = %q, want substring %q", err.Error(), "annotationPrefix must end with /")
			}
		})
	}
}

func TestLoad_DuplicateHostnamesAcrossOutputs(t *testing.T) {
	cfgYAML := `
outputs:
  - name: output-a
    hostname: [shared.example.com, unique-a.example.com]
    serviceName: svc-a
  - name: output-b
    hostname: [shared.example.com, unique-b.example.com]
    serviceName: svc-b
`

	_, err := Load(writeTestConfig(t, cfgYAML))
	if err == nil {
		t.Fatal("expected error for duplicate hostnames across outputs, got nil")
	}

	if !strings.Contains(err.Error(), "shared.example.com") {
		t.Errorf("error = %q, want it to mention the duplicate hostname", err.Error())
	}
}

func TestLoad_WhitespaceInServiceName(t *testing.T) {
	cfgYAML := `
outputs:
  - name: ws-svc
    hostname: [test.com]
    serviceName: "my service"
`

	_, err := Load(writeTestConfig(t, cfgYAML))
	if err == nil {
		t.Fatal("expected error for serviceName with whitespace, got nil")
	}

	if !strings.Contains(err.Error(), "is not a valid DNS label") {
		t.Errorf("error = %q, want substring %q", err.Error(), "is not a valid DNS label")
	}
}

func TestLoad_ValidDNSLabel(t *testing.T) {
	cfgYAML := `
outputs:
  - name: valid-dns
    hostname: [test.com]
    serviceName: my-service
`

	cfg, err := Load(writeTestConfig(t, cfgYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Outputs[0].ServiceName != "my-service" {
		t.Errorf("serviceName = %q, want %q", cfg.Outputs[0].ServiceName, "my-service")
	}
}

func TestLoad_VeryLongHostname(t *testing.T) {
	longHostname := strings.Repeat("a", 253) + ".example.com"
	cfgYAML := `
outputs:
  - name: long-host
    hostname: [` + longHostname + `]
    serviceName: svc
`

	cfg, err := Load(writeTestConfig(t, cfgYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Outputs[0].Hostnames[0] != longHostname {
		t.Errorf("hostname length = %d, want %d", len(cfg.Outputs[0].Hostnames[0]), len(longHostname))
	}
}

func TestLoad_SpecialCharactersInName(t *testing.T) {
	names := []string{"my.output", "my-output", "my_output", "output-v2.1"}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			cfgYAML := `
outputs:
  - name: ` + name + `
    hostname: [test.com]
    serviceName: svc
`

			cfg, err := Load(writeTestConfig(t, cfgYAML))
			if err != nil {
				t.Fatalf("unexpected error for output name %q: %v", name, err)
			}

			if cfg.Outputs[0].Name != name {
				t.Errorf("output name = %q, want %q", cfg.Outputs[0].Name, name)
			}
		})
	}
}
