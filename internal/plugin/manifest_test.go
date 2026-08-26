package plugin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validManifest() Manifest {
	return Manifest{
		APIVersion: APIVersionV1,
		Kind:       "Plugin",
		Metadata: Metadata{
			ID:      "test-plugin",
			Name:    "Test Plugin",
			Version: "0.1.0",
		},
		Spec: Spec{
			ExtensionType:  ExtensionTypeDataSource,
			WeKnoraVersion: ">=0.1.0",
			Entrypoint: Entrypoint{
				Type:        "process",
				Command:     []string{"./plugin"},
				GRPCAddress: "127.0.0.1:50071",
			},
			Permissions: Permissions{Network: NetworkPermission{Enabled: true}},
		},
	}
}

func TestValidateCapabilitiesRejectsInvalidForType(t *testing.T) {
	m := validManifest()
	m.Spec.Capabilities = []string{"chat"} // chat is not valid for datasource
	err := m.Validate()
	require.ErrorContains(t, err, `not valid for extension type "datasource"`)
}

func TestValidateCapabilitiesAcceptsValidForType(t *testing.T) {
	m := validManifest()
	m.Spec.Capabilities = []string{"sync", "stream"}
	require.NoError(t, m.Validate())
	require.Equal(t, []string{"sync", "stream"}, m.Spec.Capabilities)
}

func TestValidateCapabilitiesTrimsWhitespace(t *testing.T) {
	m := validManifest()
	m.Spec.Capabilities = []string{"  sync  "}
	require.NoError(t, m.Validate())
	require.Equal(t, []string{"sync"}, m.Spec.Capabilities)
}

func TestValidateVersionRangeAcceptsGte(t *testing.T) {
	m := validManifest()
	m.Spec.WeKnoraVersion = ">=0.1.0"
	require.NoError(t, m.Validate())
}

func TestValidateVersionRangeRejectsGteTooHigh(t *testing.T) {
	m := validManifest()
	m.Spec.WeKnoraVersion = ">=99.0.0"
	err := m.Validate()
	require.ErrorContains(t, err, "does not satisfy")
}

func TestValidateVersionRangeAcceptsExact(t *testing.T) {
	m := validManifest()
	m.Spec.WeKnoraVersion = "0.1.0"
	require.NoError(t, m.Validate())
}

func TestValidateVersionRangeRejectsExactMismatch(t *testing.T) {
	m := validManifest()
	m.Spec.WeKnoraVersion = "1.0.0"
	err := m.Validate()
	require.ErrorContains(t, err, "does not satisfy")
}

func TestValidateVersionRangeAcceptsRange(t *testing.T) {
	m := validManifest()
	m.Spec.WeKnoraVersion = ">=0.1.0 <1.0.0"
	require.NoError(t, m.Validate())
}

func TestValidateVersionRangeRejectsInvalid(t *testing.T) {
	m := validManifest()
	m.Spec.WeKnoraVersion = "garbage"
	err := m.Validate()
	require.ErrorContains(t, err, "unsupported version constraint")
}
