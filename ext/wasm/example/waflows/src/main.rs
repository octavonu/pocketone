//! waflows, a proof of concept: what a WhatsApp Flows endpoint needs and PocketOne's JSVM doesn't have.
//!
//! Meta's Go example (WhatsApp-Flows-Tools, examples/endpoint/go) imports, and pb_hooks has:
//!   encoding/json, encoding/base64      yes: JSON, Buffer.from(s, "base64")
//!   crypto/sha256, crypto/rand          yes: $security.sha256, $security.randomString
//!   crypto/rsa, crypto/x509, pem        no:  nothing in JSVM does RSA          -> rsa-decrypt
//!   crypto/aes, crypto/cipher (GCM)     no:  $security.encrypt is AES-GCM, but -> gcm-open, gcm-seal
//!                                            with its own key size and a random nonce, not Meta's key and IV
//!
//! So the plugin is only those two, bytes as base64 in and out; the hook does the rest:
//!
//!   const key = $wasm.call("waflows", "rsa-decrypt", { privateKey, data: body.encrypted_aes_key }).data
//!   const req = $wasm.call("waflows", "gcm-open", { key, iv: body.initial_vector, data: body.encrypted_flow_data })
//!   const flipped = Buffer.from(body.initial_vector, "base64").map((b) => ~b).toString("base64")
//!   const out = $wasm.call("waflows", "gcm-seal", { key, iv: flipped, data: Buffer.from(JSON.stringify(answer)).toString("base64") })
//!
//! Build: cargo build --release --target wasm32-wasip1
use aes_gcm::aead::{consts::U16, Aead, KeyInit};
use aes_gcm::{aes::Aes128, AesGcm};
use base64::{engine::general_purpose::STANDARD as B64, Engine};
use rsa::{pkcs8::DecodePrivateKey, Oaep, PrivateKey};
use serde_json::{json, Value};
use std::io::{self, Read};

fn main() {
    let mut raw = Vec::new();
    io::stdin().read_to_end(&mut raw).ok();
    let input: Value = serde_json::from_slice(&raw).unwrap_or_else(|_| fail("bad_input", "the input isn't JSON"));
    let data = bytes(&input, "data");
    let out = match std::env::args().nth(1).unwrap_or_default().as_str() {
        // crypto/rsa + x509 + pem: RSA-OAEP with SHA-256, the key a PKCS#8 PEM
        "rsa-decrypt" => {
            let pem = input["privateKey"].as_str().unwrap_or_else(|| fail("bad_input", "no privateKey"));
            let key = PrivateKey::from_pkcs8_pem(pem).unwrap_or_else(|_| fail("bad_key", "not a PKCS#8 RSA private key"));
            key.decrypt(Oaep::<sha2::Sha256>::new(), &data).unwrap_or_else(|_| fail("cannot_decrypt", "wrong key"))
        }
        // crypto/aes + cipher: AES-128-GCM with WhatsApp's 16-byte IV, the tag at the end of data
        f @ ("gcm-open" | "gcm-seal") => {
            let gcm = AesGcm::<Aes128, U16>::new_from_slice(&bytes(&input, "key"))
                .unwrap_or_else(|_| fail("bad_input", "key isn't 16 bytes"));
            let iv = bytes(&input, "iv");
            let iv = iv.as_slice().try_into().unwrap_or_else(|_| fail("bad_input", "iv isn't 16 bytes"));
            let r = if f == "gcm-open" { gcm.decrypt(iv, data.as_slice()) } else { gcm.encrypt(iv, data.as_slice()) };
            r.unwrap_or_else(|_| fail("cannot_decrypt", "wrong key, iv or tag"))
        }
        f => fail("bad_function", &format!("no function {f:?}")),
    };
    print!("{}", json!({ "data": B64.encode(out) }));
}

fn bytes(v: &Value, field: &str) -> Vec<u8> {
    v[field].as_str().and_then(|s| B64.decode(s).ok())
        .unwrap_or_else(|| fail("bad_input", &format!("{field} isn't base64")))
}

fn fail(code: &str, message: &str) -> ! {
    eprint!("{}", json!({ "code": code, "message": message }));
    std::process::exit(1)
}
