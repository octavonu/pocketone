//! order-xml, an example plugin in Rust: an XML order (on stdin) read into the same order JSON as edifact-orders-rust
//! (EDIFACT), so a hook can take orders in either format and store them the same way.
//!
//!   <Order>
//!     <Number>PO-4711</Number> <Date>2026-11-14</Date> <Currency>EUR</Currency>
//!     <Buyer>4012345000009</Buyer> <Supplier>4098765000006</Supplier> <Delivery>4012345000016</Delivery>
//!     <Lines>
//!       <Line number="1"><Item>4000862141404</Item><Quantity unit="PCE">10</Quantity><Price>12.50</Price></Line>
//!     </Lines>
//!   </Order>
//!
//! Parsing is the roxmltree crate's (a read-only tree, no dependencies). Build:
//!   cargo build --release --target wasm32-wasip1
use roxmltree::{Document, Node};
use serde_json::{json, Map, Value};
use std::io::{self, Read, Write};
use std::process::exit;

fn main() {
    let mut input = String::new();
    if io::stdin().read_to_string(&mut input).is_err() {
        fail("not_text", "the input isn't UTF-8 text");
    }
    if input.trim().is_empty() {
        fail("empty", "no XML in the input");
    }
    let doc = match Document::parse(&input) {
        Ok(d) => d,
        Err(e) => fail("bad_xml", &e.to_string()),
    };
    let root = doc.root_element();
    if root.tag_name().name() != "Order" {
        fail("not_order", &format!("the root element is <{}>, not <Order>", root.tag_name().name()));
    }

    let mut order = Map::new();
    for (tag, key) in [("Number", "number"), ("Date", "date"), ("Currency", "currency"),
                       ("Buyer", "buyer"), ("Supplier", "supplier"), ("Delivery", "delivery")] {
        if let Some(v) = text(root, tag) {
            order.insert(key.into(), v.into());
        }
    }
    if !order.contains_key("number") {
        fail("no_number", "no <Number>: the order has no number");
    }

    let mut lines = Vec::new();
    for line in root.descendants().filter(|n| n.has_tag_name("Line")) {
        let mut l = Map::new();
        if let Some(n) = line.attribute("number") {
            l.insert("line".into(), number(n));
        }
        if let Some(v) = text(line, "Item") {
            l.insert("item".into(), v.into());
        }
        if let Some(q) = child(line, "Quantity") {
            l.insert("quantity".into(), number(q.text().unwrap_or("").trim()));
            if let Some(u) = q.attribute("unit") {
                l.insert("unit".into(), u.into());
            }
        }
        if let Some(v) = text(line, "Price") {
            l.insert("price".into(), number(&v));
        }
        lines.push(Value::Object(l));
    }
    order.insert("lines".into(), Value::Array(lines));
    let out = serde_json::to_vec(&Value::Object(order)).unwrap();
    io::stdout().write_all(&out).ok();
}

fn child<'a, 'i>(node: Node<'a, 'i>, tag: &str) -> Option<Node<'a, 'i>> {
    node.children().find(|n| n.has_tag_name(tag))
}

/// The trimmed text of node's child element tag, if it has any.
fn text(node: Node, tag: &str) -> Option<String> {
    let t = child(node, tag)?.text()?.trim();
    (!t.is_empty()).then(|| t.to_string())
}

/// A number when the text is one, the text otherwise.
fn number(text: &str) -> Value {
    match text.parse::<f64>() {
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
