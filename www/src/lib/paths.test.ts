import assert from "node:assert/strict"
import { test } from "node:test"
import { getContactsPath, type ContactsTab } from "./paths"

test("联系人路径省略默认页签并编码目标地址", () => {
  for (const [options, expected] of [
    [undefined, "/s/server/contacts"],
    [{ tab: "contacts" }, "/s/server/contacts"],
    [{ tab: "threads" }, "/s/server/contacts?tab=threads"],
    [
      { tab: "messages", peer: "alice@example.test/my+target" },
      "/s/server/contacts?tab=messages&peer=alice%40example.test%2Fmy%2Btarget",
    ],
  ] satisfies [{ tab?: ContactsTab; peer?: string } | undefined, string][]) {
    assert.equal(getContactsPath("server", options), expected)
  }
})
