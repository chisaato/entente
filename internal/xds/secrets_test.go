package xds

import (
	"bytes"
	"testing"

	"github.com/chisaato/entente/internal/secrets"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

func TestTranslateSecrets(t *testing.T) {
	certificates := []secrets.Certificate{{Name: "z", Certificate: []byte("chain"), PrivateKey: []byte("key")}, {Name: "a", Certificate: []byte("other chain"), PrivateKey: []byte("other key")}}
	res := Translate(testModel(), nil, certificates)
	if len(res.Secrets) != 2 {
		t.Fatal("expected two SDS resources")
	}
	s := res.Secrets[0].(*tlsv3.Secret)
	if s.GetName() != "a" || !bytes.Equal(s.GetTlsCertificate().GetCertificateChain().GetInlineBytes(), certificates[1].Certificate) || !bytes.Equal(s.GetTlsCertificate().GetPrivateKey().GetInlineBytes(), certificates[1].PrivateKey) {
		t.Fatal("incorrect inline certificate or key")
	}
	v, err := res.Version()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := res.Snapshot(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GetResources(resource.SecretType)) != 2 {
		t.Fatal("snapshot missing secrets")
	}
	reordered := Translate(testModel(), nil, []secrets.Certificate{certificates[1], certificates[0]})
	v2, _ := reordered.Version()
	if v != v2 {
		t.Error("secret ordering changes version")
	}
	certificates[0].Certificate = []byte("rotated chain")
	v3, _ := Translate(testModel(), nil, certificates).Version()
	if v == v3 {
		t.Error("certificate changes must change version")
	}
	certificates[0].PrivateKey = []byte("rotated key")
	v4, _ := Translate(testModel(), nil, certificates).Version()
	if v3 == v4 {
		t.Error("private key changes must change version")
	}
}
