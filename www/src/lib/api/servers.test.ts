import assert from "node:assert/strict"
import { test } from "node:test"
import {
  $activeServerURL,
  $servers,
  getPageServerURL,
  parseServerInput,
  removeServer,
  setActiveServerURL,
  takeTokenFromHash,
  upsertServer,
} from "./servers"

test("takeTokenFromHash reads the token written by openvaala web", () => {
  const cases: { name: string; hash: string; want: string | undefined }[] = [
    { name: "token_only", hash: "#token=abc", want: "abc" },
    { name: "other_parameter", hash: "#other=1", want: undefined },
    { name: "empty_hash", hash: "", want: undefined },
    { name: "empty_token", hash: "#token=", want: undefined },
    { name: "no_leading_hash", hash: "token=abc", want: "abc" },
    {
      name: "query_escaped_token",
      hash: "#token=a%2Bb%2Fc%3D+d",
      want: "a+b/c= d",
    },
    { name: "token_among_others", hash: "#x=1&token=abc&y=2", want: "abc" },
  ]
  for (const { name, hash, want } of cases)
    assert.equal(takeTokenFromHash(hash), want, name)
})

test("getPageServerURL follows the page protocol and host", () => {
  const cases: {
    name: string
    protocol: string
    host: string
    want: string
  }[] = [
    {
      name: "http_page",
      protocol: "http:",
      host: "127.0.0.1:8790",
      want: "ws://127.0.0.1:8790/api/v1/openvaala",
    },
    {
      name: "https_page",
      protocol: "https:",
      host: "vaala.example.com",
      want: "wss://vaala.example.com/api/v1/openvaala",
    },
  ]
  for (const { name, protocol, host, want } of cases)
    assert.equal(getPageServerURL({ protocol, host }), want, name)
})

test("parseServerInput normalizes web links, ws links, and bare hosts", () => {
  const cases = [
    {
      name: "http_web_link_with_hash_token",
      rawURL: "http://10.0.0.5:8790/web/v1/#token=secret-1",
      fallback: "old",
      want: {
        ok: true as const,
        url: "ws://10.0.0.5:8790/api/v1/openvaala",
        token: "secret-1",
        defaultName: "10.0.0.5:8790",
        extractedToken: true,
      },
    },
    {
      name: "https_web_link_with_hash_token",
      rawURL: "https://gpu.vaala.dev/web/v1/#token=t%202",
      fallback: "",
      want: {
        ok: true as const,
        url: "wss://gpu.vaala.dev/api/v1/openvaala",
        token: "t 2",
        defaultName: "gpu.vaala.dev",
        extractedToken: true,
      },
    },
    {
      name: "ws_url_with_query_token",
      rawURL: "ws://10.0.0.5:8790/api/v1/openvaala?token=qtok",
      fallback: "",
      want: {
        ok: true as const,
        url: "ws://10.0.0.5:8790/api/v1/openvaala",
        token: "qtok",
        defaultName: "10.0.0.5:8790",
        extractedToken: true,
      },
    },
    {
      name: "bare_host_port_defaults_to_ws",
      rawURL: "10.0.0.8:8790",
      fallback: "manual",
      want: {
        ok: true as const,
        url: "ws://10.0.0.8:8790/api/v1/openvaala",
        token: "manual",
        defaultName: "10.0.0.8:8790",
        extractedToken: false,
      },
    },
    {
      name: "empty_input_rejected",
      rawURL: "   ",
      fallback: "",
      want: { ok: false as const, error: "required" as const },
    },
    {
      name: "unsupported_scheme_rejected",
      rawURL: "ftp://10.0.0.5:21",
      fallback: "",
      want: { ok: false as const, error: "unsupportedScheme" as const },
    },
  ]
  for (const tc of cases)
    assert.deepEqual(
      parseServerInput(tc.rawURL, tc.fallback),
      tc.want,
      tc.name
    )
})

test("upsertServer, setActiveServerURL, and removeServer manage profiles cleanly", () => {
  const pageURL = "ws://127.0.0.1:8790/api/v1/openvaala"
  $servers.set([])
  $activeServerURL.set("")

  upsertServer({
    url: "ws://10.0.0.5:8790/api/v1/openvaala",
    token: "tok-1",
    name: "Dev Box",
  })
  assert.equal($servers.get().length, 1)
  assert.equal($servers.get()[0]?.name, "Dev Box")

  setActiveServerURL("ws://10.0.0.5:8790/api/v1/openvaala", pageURL)
  assert.equal($activeServerURL.get(), "ws://10.0.0.5:8790/api/v1/openvaala")

  // Editing URL updates active target in place.
  upsertServer({
    previousURL: "ws://10.0.0.5:8790/api/v1/openvaala",
    url: "wss://gpu.vaala.dev/api/v1/openvaala",
    token: "tok-2",
    name: "GPU",
  })
  assert.equal($activeServerURL.get(), "wss://gpu.vaala.dev/api/v1/openvaala")
  assert.equal($servers.get()[0]?.url, "wss://gpu.vaala.dev/api/v1/openvaala")

  // Deleting active server falls back to default page server.
  removeServer("wss://gpu.vaala.dev/api/v1/openvaala")
  assert.equal($servers.get().length, 0)
  assert.equal($activeServerURL.get(), "")
})
