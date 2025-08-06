package sigtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSerializeVerificationKey(t *testing.T) {
	tests := []struct {
		name    string
		curve   elliptic.Curve
		wantErr bool
	}{
		{
			name:  "P256",
			curve: elliptic.P256(),
		},
		{
			name:  "P384",
			curve: elliptic.P384(),
		},
		{
			name:  "P224",
			curve: elliptic.P224(),
		},
		{
			name:  "P521",
			curve: elliptic.P521(),
		},

		// {
		// 	//  ? TODO find an invalid example?
		// 	name:    "Invalid input",
		// 	curve:   elliptic.(),
		// 	wantErr: true,
		// },
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			privKey, err := ecdsa.GenerateKey(tt.curve, rand.Reader)
			require.NoError(t, err)

			_, err = SerializeVerificationKey(&privKey.PublicKey)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSerializeSigningKey(t *testing.T) {

	// Panic can happen from unwanted side effects when nil and empty Keys are passed
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("SerializeSigningKey() panics: %v", r)
		}
	}()

	t.Run("Key Empty", func(t *testing.T) {
		emptyKey := &ecdsa.PrivateKey{}
		_, err := SerializeSigningKey(emptyKey)
		require.Error(t, err)
	})
	t.Run("Key is nil", func(t *testing.T) {
		_, err := SerializeSigningKey(nil)
		require.Error(t, err)
	})

	t.Run("Key OK", func(t *testing.T) {
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		key, err := SerializeSigningKey(privateKey)
		fmt.Println(key)
		require.NoError(t, err)
	})

}

func TestParseSigningKey(t *testing.T) {
	t.Run("Key is nil", func(t *testing.T) {
		_, err := ParseSigningKey(nil)
		require.Error(t, err)
	})
	t.Run("Key OK", func(t *testing.T) {
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		key, err := SerializeSigningKey(privateKey)
		require.NoError(t, err)
		_, err = ParseSigningKey(key)
		require.NoError(t, err)
	})
}
