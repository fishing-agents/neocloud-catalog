// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package tests renders the chart with helm (skipped without it) and checks that
// the rendered config.toml validates and every file it names is mounted.
package tests

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/fishman/neocloud-catalog/service/internal/config"
)

type secretKeyRef struct{ Name, Key string }

type envVar struct {
	Name      string
	Value     string
	ValueFrom *struct {
		SecretKeyRef *secretKeyRef `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

type keyPath struct{ Key, Path string }

type volume struct {
	Name      string
	ConfigMap *struct{ Name string } `yaml:"configMap"`
	Secret    *struct {
		SecretName string `yaml:"secretName"`
		Items      []keyPath
	}
	Projected *struct {
		Sources []struct {
			ConfigMap *struct{ Name string } `yaml:"configMap"`
			Secret    *struct {
				Name  string
				Items []keyPath
			}
		}
	}
	Image *struct{ Reference string }
}

type container struct {
	Name, Image string
	Args        []string
	Env         []envVar
	Ports       []struct {
		ContainerPort int `yaml:"containerPort"`
	}
	VolumeMounts []struct {
		Name      string
		MountPath string `yaml:"mountPath"`
	} `yaml:"volumeMounts"`
}

type podSpec struct {
	Containers       []container
	Volumes          []volume
	ImagePullSecrets []struct{ Name string } `yaml:"imagePullSecrets"`
}

type object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string
	Metadata   struct {
		Name        string
		Labels      map[string]string
		Annotations map[string]string
	}
	Data map[string]string
	Spec struct {
		Template struct {
			Metadata struct{ Annotations map[string]string }
			Spec     podSpec
		}
		Selector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		}
	}
}

type rendered struct {
	out     string
	objects []object
}

func helm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin := os.Getenv("HELM")
	if bin == "" {
		bin = "helm"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("helm not installed")
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	return string(out), err
}

func template(t *testing.T, kubeVersion string, args ...string) (string, error) {
	t.Helper()
	return helm(t, append([]string{"template", "test", "..", "--kube-version", kubeVersion}, args...)...)
}

func render(t *testing.T, args ...string) rendered {
	t.Helper()
	out, err := template(t, "1.36.0", args...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	r := rendered{out: out}
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var o object
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if o.Kind != "" {
			if o.APIVersion == "" {
				t.Fatalf("%s without apiVersion (a template comment glued onto it?):\n%s", o.Kind, out)
			}
			r.objects = append(r.objects, o)
		}
	}
	return r
}

func (r rendered) get(t *testing.T, kind, name string) object {
	t.Helper()
	for _, o := range r.objects {
		if o.Kind == kind && o.Metadata.Name == name {
			return o
		}
	}
	t.Fatalf("no %s %s in:\n%s", kind, name, r.out)
	return object{}
}

func (r rendered) has(kind string) bool {
	return slices.ContainsFunc(r.objects, func(o object) bool { return o.Kind == kind })
}

// config loads the rendered config.toml as the pod does: defaults, the ConfigMap dir, the DSN from env.
func (r rendered) config(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	toml := r.get(t, "ConfigMap", "catalog-service").Data["config.toml"]
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOCLOUD_CATALOG_DATABASE_DSN", "postgres://from-the-secret")
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("rendered config.toml: %v\n%s", err, toml)
	}
	return cfg
}

// file returns where the pod of o gets file p: "configmap <name> <key>" or
// "secret <name> <key>". The key must exist in the rendered source.
func (r rendered) file(t *testing.T, o object, p string) string {
	t.Helper()
	c := o.Spec.Template.Spec.Containers[0]
	for _, m := range c.VolumeMounts {
		rel, ok := strings.CutPrefix(p, m.MountPath+"/")
		if !ok {
			continue
		}
		for _, v := range o.Spec.Template.Spec.Volumes {
			switch {
			case v.Name != m.Name:
			case v.ConfigMap != nil:
				if _, ok := r.get(t, "ConfigMap", v.ConfigMap.Name).Data[rel]; !ok {
					t.Fatalf("%s: ConfigMap %s has no %s", p, v.ConfigMap.Name, rel)
				}
				return "configmap " + v.ConfigMap.Name + " " + rel
			case v.Secret != nil:
				for _, it := range v.Secret.Items {
					if it.Path == rel {
						return "secret " + v.Secret.SecretName + " " + it.Key
					}
				}
				t.Fatalf("%s: Secret volume %s has no item at %s", p, v.Secret.SecretName, rel)
			case v.Projected != nil:
				for _, src := range v.Projected.Sources {
					if src.Secret != nil {
						for _, it := range src.Secret.Items {
							if it.Path == rel {
								return "secret " + src.Secret.Name + " " + it.Key
							}
						}
					}
					if src.ConfigMap != nil {
						if _, ok := r.get(t, "ConfigMap", src.ConfigMap.Name).Data[rel]; ok {
							return "configmap " + src.ConfigMap.Name + " " + rel
						}
					}
				}
				t.Fatalf("%s: projected volume %s has no %s", p, v.Name, rel)
			}
		}
	}
	t.Fatalf("%s is not mounted in %s", p, o.Metadata.Name)
	return ""
}

func env(c container, name string) *envVar {
	for _, e := range c.Env {
		if e.Name == name {
			return &e
		}
	}
	return nil
}

func secretEnv(t *testing.T, c container, name string) secretKeyRef {
	t.Helper()
	e := env(c, name)
	if e == nil || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.Value != "" {
		t.Fatalf("%s: %s is not from a Secret: %+v", c.Name, name, e)
	}
	return *e.ValueFrom.SecretKeyRef
}

func TestDefaultMode(t *testing.T) {
	r := render(t)
	if strings.Contains(r.out, "postgres://") {
		t.Errorf("a DSN is rendered:\n%s", r.out)
	}
	if r.has("ServiceMonitor") {
		t.Error("ServiceMonitor rendered while disabled")
	}
	cfg := r.config(t)
	if cfg.Upstream.Ref != "" || cfg.Release.Ref != "" || cfg.HuggingFace.TokenFile != "" {
		t.Errorf("default mode config: upstream.ref %q, release.ref %q, token_file %q", cfg.Upstream.Ref, cfg.Release.Ref, cfg.HuggingFace.TokenFile)
	}
	if cfg.Bootstrap.DataDir != "/data" {
		t.Errorf("bootstrap.data_dir = %q, want the image's /data", cfg.Bootstrap.DataDir)
	}

	d := r.get(t, "Deployment", "catalog-service")
	c := d.Spec.Template.Spec.Containers[0]
	if want := []string{"serve"}; !slices.Equal(c.Args, want) {
		t.Errorf("args = %q, want %q", c.Args, want)
	}
	if got := secretEnv(t, c, "NEOCLOUD_CATALOG_DATABASE_DSN"); got != (secretKeyRef{"catalog-service-db", "dsn"}) {
		t.Errorf("DSN from %+v", got)
	}
	if e := env(c, "NEOCLOUD_CATALOG_CONFIG_DIR"); e == nil || r.file(t, d, e.Value+"/config.toml") != "configmap catalog-service config.toml" {
		t.Errorf("NEOCLOUD_CATALOG_CONFIG_DIR = %+v", e)
	}
	for _, name := range []string{"COSIGN_PASSWORD", "DOCKER_CONFIG"} {
		if env(c, name) != nil {
			t.Errorf("%s set without its Secret", name)
		}
	}
	if _, ok := d.Spec.Template.Metadata.Annotations["checksum/config"]; !ok {
		t.Error("no checksum/config annotation: a config change would not roll the pods")
	}

	checkMigrate(t, r, c.Image, secretKeyRef{"catalog-service-db", "dsn"})
}

// checkMigrate: the hook Job migrates with the Deployment's image, the DSN only in its env.
func checkMigrate(t *testing.T, r rendered, image string, dsn secretKeyRef) {
	t.Helper()
	j := r.get(t, "Job", "catalog-service-migrate")
	if got := j.Metadata.Annotations["helm.sh/hook"]; got != "pre-install,pre-upgrade" {
		t.Errorf("migrate hook = %q", got)
	}
	c := j.Spec.Template.Spec.Containers[0]
	if got := secretEnv(t, c, "NEOCLOUD_CATALOG_DATABASE_DSN"); got != dsn {
		t.Errorf("migrate DSN from %+v, want %+v", got, dsn)
	}
	if want := []string{"migrate", "apply", "--config", "file:///etc/atlas/atlas.hcl", "--env", "catalog"}; !slices.Equal(c.Args, want) {
		t.Errorf("migrate args = %q, want %q", c.Args, want)
	}
	if src := r.file(t, j, "/etc/atlas/atlas.hcl"); src != "configmap catalog-service-migrate atlas.hcl" {
		t.Errorf("atlas.hcl from %s", src)
	}
	cm := r.get(t, "ConfigMap", "catalog-service-migrate")
	cmWeight, _ := strconv.Atoi(cm.Metadata.Annotations["helm.sh/hook-weight"])
	jobWeight, _ := strconv.Atoi(j.Metadata.Annotations["helm.sh/hook-weight"])
	if cm.Metadata.Annotations["helm.sh/hook"] != "pre-install,pre-upgrade" || cmWeight >= jobWeight {
		t.Errorf("atlas ConfigMap must be a hook created before the Job: %v / %v", cm.Metadata.Annotations, j.Metadata.Annotations)
	}
	hcl := cm.Data["atlas.hcl"]
	if !strings.Contains(hcl, `getenv("NEOCLOUD_CATALOG_DATABASE_DSN")`) {
		t.Errorf("atlas.hcl does not read the DSN from the environment:\n%s", hcl)
	}
	var mount string
	for _, m := range c.VolumeMounts {
		for _, v := range j.Spec.Template.Spec.Volumes {
			if v.Name == m.Name && v.Image != nil {
				if v.Image.Reference != image {
					t.Errorf("migrate mounts image %q, Deployment runs %q", v.Image.Reference, image)
				}
				mount = m.MountPath
			}
		}
	}
	// The Dockerfile copies the migrations to /migrations.
	if dir := `"file://` + path.Join(mount, "migrations") + `"`; mount == "" || !strings.Contains(hcl, dir) {
		t.Errorf("atlas.hcl migration dir is not %s in the mounted service image:\n%s", dir, hcl)
	}
}

func TestOperatorMode(t *testing.T) {
	r := render(t, "-f", "fixtures/operator.yaml")
	cfg := r.config(t)
	if cfg.Upstream.Ref == "" || cfg.Upstream.OverlayRef == "" || !cfg.Upstream.PlainHTTP || cfg.Auth.IssuerURL == "" {
		t.Errorf("operator config lost values: %+v %+v", cfg.Upstream, cfg.Auth)
	}
	d := r.get(t, "Deployment", "catalog-service")
	c := d.Spec.Template.Spec.Containers[0]
	const image = "registry.example.com/catalog-service:20261009@sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
	if c.Image != image {
		t.Errorf("image = %q, want %q", c.Image, image)
	}
	for p, want := range map[string]string{
		cfg.Release.Key:               "secret operator-cosign cosign.key",
		cfg.Upstream.OverlayPublicKey: "configmap catalog-service upstream-overlay-public-key.pem",
		cfg.Upstream.TrustedRoot:      "configmap catalog-service upstream-trusted-root.json",
	} {
		if got := r.file(t, d, p); got != want {
			t.Errorf("%s from %s, want %s", p, got, want)
		}
	}
	if cfg.Upstream.PublicKey != "" {
		t.Errorf("upstream.public_key = %q though the release is trusted keyless", cfg.Upstream.PublicKey)
	}
	cm := r.get(t, "ConfigMap", "catalog-service")
	if !strings.HasPrefix(cm.Data["upstream-overlay-public-key.pem"], "-----BEGIN PUBLIC KEY-----\n") {
		t.Errorf("overlay public key content: %q", cm.Data["upstream-overlay-public-key.pem"])
	}
	if got := secretEnv(t, c, "COSIGN_PASSWORD"); got != (secretKeyRef{"operator-cosign", "cosign.password"}) {
		t.Errorf("COSIGN_PASSWORD from %+v", got)
	}
	e := env(c, "DOCKER_CONFIG")
	if e == nil {
		t.Fatal("no DOCKER_CONFIG with registryAuth.secretName")
	}
	if got := r.file(t, d, e.Value+"/config.json"); got != "secret operator-registry .dockerconfigjson" {
		t.Errorf("docker config.json from %s", got)
	}
	sm := r.get(t, "ServiceMonitor", "catalog-service")
	svc := r.get(t, "Service", "catalog-service")
	for k, v := range sm.Spec.Selector.MatchLabels {
		if svc.Metadata.Labels[k] != v {
			t.Errorf("ServiceMonitor selects %s=%s; the Service is labelled %v", k, v, svc.Metadata.Labels)
		}
	}
	checkMigrate(t, r, image, secretKeyRef{"operator-catalog-db", "url"})
}

func TestHuggingFaceToken(t *testing.T) {
	token := []string{"--set", "huggingface.tokenSecretName=hf", "--set", "huggingface.tokenSecretKey=hf_token"}
	r := render(t, token...)
	if got := r.file(t, r.get(t, "Deployment", "catalog-service"), r.config(t).HuggingFace.TokenFile); got != "secret hf hf_token" {
		t.Errorf("huggingface.token_file from %s", got)
	}

	// An operator deployment's model imports read with the token too.
	r = render(t, append([]string{"-f", "fixtures/operator.yaml"}, token...)...)
	if got := r.file(t, r.get(t, "Deployment", "catalog-service"), r.config(t).HuggingFace.TokenFile); got != "secret hf hf_token" {
		t.Errorf("operator mode: huggingface.token_file from %s", got)
	}
}

// Webhook subscribers, secrets included, come from a Secret next to config.toml, loaded after it.
func TestWebhookSubscribers(t *testing.T) {
	r := render(t, "--set", "webhooks.secretName=hooks", "--set", "webhooks.secretKey=subscribers.toml")
	d := r.get(t, "Deployment", "catalog-service")
	if got := r.file(t, d, "/etc/catalog-service/webhooks.toml"); got != "secret hooks subscribers.toml" {
		t.Errorf("webhooks.toml from %s", got)
	}
	if got := r.file(t, d, "/etc/catalog-service/config.toml"); got != "configmap catalog-service config.toml" {
		t.Errorf("config.toml from %s", got)
	}
	if strings.Contains(r.get(t, "ConfigMap", "catalog-service").Data["config.toml"], "webhooks") {
		t.Error("the ConfigMap holds webhook settings")
	}
}

// Both pods carry the pull secrets: the migrate Job mounts the service image too.
func TestImagePullSecrets(t *testing.T) {
	r := render(t, "--set", "imagePullSecrets[0].name=catalog-registry")
	for _, kind := range [][2]string{{"Deployment", "catalog-service"}, {"Job", "catalog-service-migrate"}} {
		got := r.get(t, kind[0], kind[1]).Spec.Template.Spec.ImagePullSecrets
		if len(got) != 1 || got[0].Name != "catalog-registry" {
			t.Errorf("%s imagePullSecrets = %+v", kind[0], got)
		}
	}
}

// Listen addresses follow the container ports even when config sets them.
func TestPortsOwnListenAddresses(t *testing.T) {
	r := render(t, "--set", "service.port=9090", "--set", "service.metricsPort=9091", "--set", "config.server.addr=0.0.0.0:1")
	cfg := r.config(t)
	if cfg.Server.Addr != "0.0.0.0:9090" || cfg.Telemetry.MetricsAddr != "0.0.0.0:9091" {
		t.Errorf("addr %q metrics %q, want ports 9090 and 9091", cfg.Server.Addr, cfg.Telemetry.MetricsAddr)
	}
	var ports []int
	for _, p := range r.get(t, "Deployment", "catalog-service").Spec.Template.Spec.Containers[0].Ports {
		ports = append(ports, p.ContainerPort)
	}
	if !slices.Equal(ports, []int{9090, 9091}) {
		t.Errorf("container ports %v", ports)
	}
}

func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		name, kube string
		args       []string
		want       string
	}{
		{"no DSN Secret", "1.36.0", []string{"--set", "database.dsnSecretName="}, "database.dsnSecretName is required"},
		{"publish without a key", "1.36.0", []string{"--set", "config.release.ref=r.example/c:v4"}, "signing.secretName is required"},
		{"trust without upstream", "1.36.0", []string{"--set", "upstreamTrust.publicKey=pem"}, "upstreamTrust.* needs config.upstream.ref"},
		{"trusted root without upstream", "1.36.0", []string{"--set", "upstreamTrust.trustedRoot={}"}, "upstreamTrust.* needs config.upstream.ref"},
		{"kubernetes 1.35", "1.35.0", nil, "kubeVersion"},
	} {
		out, err := template(t, tc.kube, tc.args...)
		if err == nil || !strings.Contains(out, tc.want) {
			t.Errorf("%s: want failure mentioning %q, got err=%v\n%s", tc.name, tc.want, err, out)
		}
	}
}

func TestLint(t *testing.T) {
	for _, args := range [][]string{nil, {"-f", "fixtures/operator.yaml"}} {
		out, err := helm(t, append([]string{"lint", "..", "--strict", "--kube-version", "1.36.0"}, args...)...)
		if err != nil || !strings.Contains(out, "0 chart(s) failed") {
			t.Errorf("helm lint %q: %v\n%s", args, err, out)
		}
	}
}
