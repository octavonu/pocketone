// The module gets what the hook gives it and returns a value; the hook decides what to save. Calls are async: the
// module runs off the app's JS VM, so the app serves other requests meanwhile.
onRecordCreate(async (e) => {
  try {
    const out = await $wasm.run.async("summary", { title: e.record.get("title"), body: e.record.get("body") })
    e.record.set("summary", out.text)
    e.record.set("words", out.words)
  } catch (err) {
    // err.code, err.message, err.retryable, err.module
    if (err.code !== "empty_body") throw err
  }
  return e.next()
}, "posts")

// A module never reaches the network: the hook calls the outside API and passes the result in.
//   GET /api/summarize?url=https://example.com
routerAdd("GET", "/api/summarize", async (e) => {
  const url = e.request.url.query().get("url")
  const res = $http.send({ url: url, timeout: 10 })
  const body = toString(res.body).replace(/<[^>]*>/g, " ")
  return e.json(200, await $wasm.run.async("summary", { title: url, body: body }))
}, $apis.requireSuperuserAuth())
