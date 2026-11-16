// Command crypto is an example plugin: AES-256-GCM, the kind of helper an app used to run with $os.exec. The key comes
// in with the input, from the hook; the plugin can't keep it or send it anywhere.
//
//	encrypt  stdin {"key": "<32 bytes, base64>", "text": "..."}  ->  {"data": "<nonce + ciphertext, base64>"}
//	decrypt  stdin {"key": "...", "data": "..."}                 ->  {"text": "..."}
//
// Every encryption takes a fresh random nonce (WASI's random_get: the host's crypto/rand), so the same text gives a
// different data each time. Build: GOOS=wasip1 GOARCH=wasm go build -o crypto.wasm .
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
)

func main() {
	var in struct {
		Key  string `json:"key"`
		Text string `json:"text"`
		Data string `json:"data"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fail("bad_input", "the input isn't JSON: "+err.Error())
	}
	key, err := base64.StdEncoding.DecodeString(in.Key)
	if err != nil || len(key) != 32 {
		fail("bad_key", "key: 32 bytes, base64 (openssl rand -base64 32)")
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)

	switch os.Args[1] {
	case "encrypt":
		nonce := make([]byte, gcm.NonceSize())
		rand.Read(nonce)
		sealed := gcm.Seal(nonce, nonce, []byte(in.Text), nil)
		json.NewEncoder(os.Stdout).Encode(map[string]string{"data": base64.StdEncoding.EncodeToString(sealed)})
	case "decrypt":
		sealed, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil || len(sealed) < gcm.NonceSize() {
			fail("bad_data", "data: base64, from encrypt")
		}
		text, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
		if err != nil {
			fail("bad_data", "wrong key, or the data was changed")
		}
		json.NewEncoder(os.Stdout).Encode(map[string]string{"text": string(text)})
	default:
		fail("no_such_function", os.Args[1])
	}
}

func fail(code, message string) {
	b, _ := json.Marshal(map[string]string{"code": code, "message": message})
	os.Stderr.Write(b)
	os.Exit(1)
}
