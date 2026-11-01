import assert from "node:assert/strict"
import { test } from "node:test"
import { getPageServerURL, takeTokenFromHash } from "./servers"

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
