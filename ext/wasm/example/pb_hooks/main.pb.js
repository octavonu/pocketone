// The plugin gets what the hook gives it and returns a value; the hook decides what to save.
onRecordCreate((e) => {
  try {
    const out = $wasm.run("summary", { title: e.record.get("title"), body: e.record.get("body") })
    e.record.set("summary", out.text)
    e.record.set("words", out.words)
  } catch (err) {
    // err.code, err.message, err.retryable, err.plugin
    if (err.code !== "empty_body") throw err
  }
  e.next()
}, "posts")

// A plugin never reaches the network: the hook calls the outside API and passes the result in.
//   GET /api/summarize?url=https://example.com
routerAdd("GET", "/api/summarize", (e) => {
  const url = e.request.url.query().get("url")
  const res = $http.send({ url: url, timeout: 10 })
  const body = toString(res.body).replace(/<[^>]*>/g, " ")
  return e.json(200, $wasm.run("summary", { title: url, body: body }))
}, $apis.requireSuperuserAuth())
