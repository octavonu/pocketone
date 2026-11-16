//! edifact-orders-rust, an example plugin in Rust: an EDIFACT ORDERS message (raw text on stdin, as the hook got it)
//! read into an order (JSON on stdout), every segment of a usual order kept: the interchange (UNB), the document (BGM,
//! DTM, RFF, CUX), the parties with their addresses and contacts (NAD, CTA, COM), and each line with its article
//! numbers, description, quantity, dates, amount, prices and allowances (LIN, PIA, IMD, QTY, DTM, MOA, PRI, ALC, PCD),
//! and free text (FTX). The counts in UNT and UNZ are checked. Codes stay as the message has them (BY, AAA, EN...); a
//! segment the plugin doesn't read is listed in "unhandled", never dropped silently.
//!
//! buyer, supplier, invoicee, delivery (the parties' ids) and lines[].price (the net price) are the fields order-xml
//! gives too. No validation against a directory. Parsing (separators, UNA, the release character) is edifact-rs's.
//! Build:
//!   cargo build --release --target wasm32-wasip1
use serde_json::{json, Map, Value};
use std::io::{self, Read, Write};
use std::process::exit;

type Obj = Map<String, Value>;

fn main() {
    let mut input = Vec::new();
    io::stdin().read_to_end(&mut input).ok();
    if input.iter().all(|b| b.is_ascii_whitespace()) {
        fail("empty", "no EDIFACT message in the input");
    }
    let segments = match edifact_rs::from_bytes(&input).collect::<Result<Vec<_>, _>>() {
        Ok(s) => s,
        Err(e) => fail("bad_edifact", &e.to_string()),
    };

    let mut order = Obj::new();
    let mut parties: Vec<Obj> = Vec::new();
    let mut lines: Vec<Obj> = Vec::new();
    let mut unhandled: Vec<String> = Vec::new();
    let mut warnings: Vec<String> = Vec::new();
    let (mut kind, mut in_message, mut counted, mut messages) = (String::new(), false, 0, 0);
    // what the next CTA, COM, PCD or MOA belongs to: the last party, contact, line or allowance
    let mut in_allowance = false;

    for s in &segments {
        let c = |e: usize, n: usize| s.component_str(e, n).unwrap_or("").to_string();
        if s.tag() == "UNH" {
            in_message = true;
            messages += 1;
        }
        if in_message {
            counted += 1;
        }
        if s.tag() != "ALC" && s.tag() != "PCD" && s.tag() != "MOA" {
            in_allowance = false;
        }
        match s.tag() {
            "UNA" | "UNS" => {} // separators; the section between header and lines
            "UNB" => {
                let mut i = Obj::new();
                set(&mut i, "syntax", join(&[c(0, 0), c(0, 1)]));
                i.insert("sender".into(), id(&c(1, 0), &c(1, 1)));
                i.insert("recipient".into(), id(&c(2, 0), &c(2, 1)));
                set(&mut i, "date", stamp(&c(3, 0), &c(3, 1)));
                set(&mut i, "reference", c(4, 0));
                order.insert("interchange".into(), i.into());
            }
            "UNH" => {
                kind = c(1, 0);
                let mut m = Obj::new();
                set(&mut m, "reference", c(0, 0));
                set(&mut m, "type", join(&[c(1, 0), c(1, 1), c(1, 2), c(1, 3)]));
                order.insert("message".into(), m.into());
            }
            "BGM" => {
                set(&mut order, "type", c(0, 0)); // 220: order
                set(&mut order, "number", c(1, 0));
                set(&mut order, "function", c(2, 0)); // 9: original
            }
            "DTM" => {
                let (key, value) = (c(0, 0), date(&c(0, 1), &c(0, 2)));
                match lines.last_mut() {
                    Some(line) if c(0, 0) == "2" => set(line, "deliveryDate", value), // requested delivery
                    Some(line) => add(line, "dates", &key, value.into()),
                    None if c(0, 0) == "137" => set(&mut order, "date", value), // document date
                    None => add(&mut order, "dates", &key, value.into()),
                }
            }
            "RFF" => add(&mut order, "references", &c(0, 0), c(0, 1).into()),
            "FTX" => { // free text: the order's, or the line's after a LIN
                let note = json!({"subject": c(0, 0), "text": join_with(" ", &(0..5).map(|n| c(3, n)).collect::<Vec<_>>())});
                match lines.last_mut() {
                    Some(line) => push(line, "notes", note),
                    None => push(&mut order, "notes", note),
                }
            }
            "CUX" => set(&mut order, "currency", c(0, 1)),
            "NAD" => {
                let role = c(0, 0);
                let mut p = Obj::new();
                set(&mut p, "role", role.clone());
                set(&mut p, "id", c(1, 0));
                set(&mut p, "agency", c(1, 2)); // 9: GS1 (a GLN)
                set(&mut p, "name", join_with(" ", &[c(3, 0), c(3, 1), c(3, 2)]));
                set(&mut p, "street", join_with(" ", &[c(4, 0), c(4, 1), c(4, 2)]));
                set(&mut p, "city", c(5, 0));
                set(&mut p, "region", c(6, 0));
                set(&mut p, "postcode", c(7, 0));
                set(&mut p, "country", c(8, 0));
                parties.push(p);
                let field = match role.as_str() {
                    "BY" => "buyer",
                    "SU" => "supplier",
                    "IV" => "invoicee",
                    "DP" => "delivery",
                    _ => "",
                };
                if !field.is_empty() {
                    set(&mut order, field, c(1, 0));
                }
            }
            "CTA" => match parties.last_mut() {
                Some(p) => {
                    let mut contact = Obj::new();
                    set(&mut contact, "function", c(0, 0)); // OC: order contact
                    set(&mut contact, "name", join_with(" ", &[c(1, 0), c(1, 1)]));
                    push(p, "contacts", contact.into());
                }
                None => unhandled.push("CTA".into()),
            },
            "COM" => {
                let com = json!({"channel": c(0, 1), "value": c(0, 0)}); // EM: email, TE: phone
                // the last contact's, or the party's when it has no contact
                match parties.last_mut() {
                    Some(p) => match p.get_mut("contacts").and_then(|v| v.as_array_mut()).and_then(|a| a.last_mut()) {
                        Some(Value::Object(contact)) => push(contact, "communications", com),
                        _ => push(p, "communications", com),
                    },
                    None => unhandled.push("COM".into()),
                }
            }
            "LIN" => {
                let mut line = Obj::new();
                line.insert("line".into(), number(&c(0, 0)));
                set(&mut line, "item", c(2, 0));
                set(&mut line, "itemType", c(2, 1)); // EN: EAN/GTIN
                lines.push(line);
            }
            "PIA" | "IMD" | "QTY" | "PRI" | "ALC" | "PCD" | "MOA" if lines.is_empty() => unhandled.push(s.tag().into()),
            "PIA" => {
                let line = lines.last_mut().unwrap();
                push(line, "ids", json!({"id": c(1, 0), "type": c(1, 1)})); // SA: supplier's, IN: buyer's
            }
            "IMD" => set(lines.last_mut().unwrap(), "description", join_with(" ", &[c(2, 3), c(2, 4)])),
            "QTY" => {
                let line = lines.last_mut().unwrap();
                if c(0, 0) == "21" { // ordered quantity
                    line.insert("quantity".into(), number(&c(0, 1)));
                    set(line, "unit", c(0, 2));
                } else {
                    add(line, "quantities", &c(0, 0), number(&c(0, 1)));
                }
            }
            "PRI" => {
                let line = lines.last_mut().unwrap();
                let mut p = Obj::new();
                set(&mut p, "type", c(0, 0)); // AAA: net, AAB: gross
                p.insert("amount".into(), number(&c(0, 1)));
                if !c(0, 4).is_empty() {
                    p.insert("per".into(), number(&c(0, 4)));
                }
                set(&mut p, "unit", c(0, 5));
                // price is the net price (AAA); another one only while there's no net price
                if c(0, 0) == "AAA" || !line.contains_key("price") {
                    line.insert("price".into(), number(&c(0, 1)));
                }
                push(line, "prices", p.into());
            }
            "ALC" => {
                let line = lines.last_mut().unwrap();
                let mut a = Obj::new();
                set(&mut a, "type", c(0, 0)); // A: allowance, C: charge
                set(&mut a, "code", c(4, 0));
                push(line, "allowances", a.into());
                in_allowance = true;
            }
            "PCD" | "MOA" if in_allowance => {
                let a = lines.last_mut().unwrap()["allowances"].as_array_mut().unwrap().last_mut().unwrap();
                let a = a.as_object_mut().unwrap();
                let key = if s.tag() == "PCD" { "percent" } else { "amount" };
                a.insert(key.into(), number(&c(0, 1)));
            }
            "MOA" => {
                let line = lines.last_mut().unwrap();
                if c(0, 0) == "203" { // the line's amount
                    line.insert("amount".into(), number(&c(0, 1)));
                } else {
                    add(line, "amounts", &c(0, 0), number(&c(0, 1)));
                }
            }
            "UNT" => {
                in_message = false;
                let said = c(0, 0).parse::<usize>().unwrap_or(0);
                if said != counted {
                    warnings.push(format!("UNT says {said} segments, the message has {counted}"));
                }
                counted = 0;
            }
            "UNZ" => {
                let said = c(0, 0).parse::<usize>().unwrap_or(0);
                if said != messages {
                    warnings.push(format!("UNZ says {said} messages, the interchange has {messages}"));
                }
            }
            other => unhandled.push(other.into()),
        }
    }
    if kind != "ORDERS" {
        fail("not_orders", &format!("the message is {}, not ORDERS", if kind.is_empty() { "no message (no UNH)" } else { &kind }));
    }
    if !order.contains_key("number") {
        fail("no_number", "no BGM segment: the order has no number");
    }
    order.insert("parties".into(), json!(parties));
    order.insert("lines".into(), json!(lines));
    if !unhandled.is_empty() {
        order.insert("unhandled".into(), json!(unhandled));
    }
    if !warnings.is_empty() {
        order.insert("warnings".into(), json!(warnings));
    }
    let out = serde_json::to_vec(&Value::Object(order)).unwrap();
    io::stdout().write_all(&out).ok();
}

fn set(o: &mut Obj, key: &str, value: String) {
    if !value.is_empty() {
        o.insert(key.into(), value.into());
    }
}

/// add puts key: value in the object o[field], made when missing.
fn add(o: &mut Obj, field: &str, key: &str, value: Value) {
    if let Some(m) = o.entry(field).or_insert_with(|| json!({})).as_object_mut() {
        m.insert(key.into(), value);
    }
}

/// push appends value to the array o[field], made when missing.
fn push(o: &mut Obj, field: &str, value: Value) {
    if let Some(a) = o.entry(field).or_insert_with(|| json!([])).as_array_mut() {
        a.push(value);
    }
}

fn id(value: &str, qualifier: &str) -> Value {
    json!({"id": value, "qualifier": qualifier}) // 14: GS1
}

fn join(parts: &[String]) -> String {
    let parts: Vec<&str> = parts.iter().map(|s| s.as_str()).filter(|s| !s.is_empty()).collect();
    parts.join(":")
}

fn join_with(sep: &str, parts: &[String]) -> String {
    let parts: Vec<&str> = parts.iter().map(|s| s.as_str()).filter(|s| !s.is_empty()).collect();
    parts.join(sep)
}

/// A DTM date as YYYY-MM-DD when its format is 102 (CCYYMMDD), YYYY-MM-DDTHH:MM for 203; as it came otherwise.
fn date(value: &str, format: &str) -> String {
    match (format, value.len()) {
        ("102", 8) => format!("{}-{}-{}", &value[..4], &value[4..6], &value[6..]),
        ("203", 12) => format!("{}-{}-{}T{}:{}", &value[..4], &value[4..6], &value[6..8], &value[8..10], &value[10..]),
        _ => value.to_string(),
    }
}

/// UNB's date (YYMMDD) and time (HHMM) as YYYY-MM-DDTHH:MM.
fn stamp(d: &str, t: &str) -> String {
    if d.len() != 6 {
        return d.to_string();
    }
    let day = format!("20{}-{}-{}", &d[..2], &d[2..4], &d[4..]);
    if t.len() == 4 { format!("{day}T{}:{}", &t[..2], &t[2..]) } else { day }
}

/// A number when the text is one (a comma as the decimal mark too), the text otherwise.
fn number(text: &str) -> Value {
    match text.replace(',', ".").parse::<f64>() {
        Ok(n) if n.fract() == 0.0 && n.abs() < 1e15 => json!(n as i64),
        Ok(n) => json!(n),
        Err(_) => json!(text),
    }
}

fn fail(code: &str, message: &str) -> ! {
    let e = json!({"code": code, "message": message});
    io::stderr().write_all(e.to_string().as_bytes()).ok();
    exit(1)
}
