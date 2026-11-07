import assert from "node:assert/strict"
import { test } from "node:test"
import type { Content } from "./api/protocol.gen"
import {
  $drafts,
  getDraft,
  getDraftText,
  newConversationDraftID,
  replaceDraftText,
  removeSentContent,
  setDraft,
  removeSentText,
} from "./drafts"

test("draft identities isolate servers, existing conversations and new conversations", async () => {
  $drafts.set({})
  const targets = [
    { serverID: "server-A", conversationID: "A", text: "draft-A" },
    { serverID: "server-A", conversationID: "B", text: "draft-B" },
    { serverID: "server-B", conversationID: "A", text: "other-server" },
    {
      serverID: "server-A",
      conversationID: newConversationDraftID,
      text: "new-A",
    },
    {
      serverID: "server-B",
      conversationID: newConversationDraftID,
      text: "new-B",
    },
  ]
  for (const target of targets)
    setDraft(target.serverID, target.conversationID, {
      content: [{ kind: "text", text: { text: target.text } }],
    })
  // 1. 发送和附件回包都使用发起时捕获的身份。
  const sent = getDraft("server-A", "A").content
  await Promise.resolve()
  setDraft("server-A", "A", removeSentContent(getDraft("server-A", "A"), sent))
  setDraft("server-A", "A", {
    content: [...getDraft("server-A", "A").content, image],
  })
  // 2. 其他对话和新对话草稿保持原内容。
  assert.deepEqual(getDraft("server-A", "A").content, [image])
  for (const target of targets.slice(1))
    assert.equal(
      getDraftText(getDraft(target.serverID, target.conversationID).content),
      target.text
    )
  assert.deepEqual(getDraft("server-A", "loading-existing").content, [])
})

test("removeSentText keeps only what the user added during sending", () => {
  const cases: {
    name: string
    draft: string
    sentText: string
    want: string
  }[] = [
    { name: "unchanged", draft: "你好", sentText: "你好", want: "" },
    {
      name: "appended_text",
      draft: "你好 继续写",
      sentText: "你好",
      want: "继续写",
    },
    {
      name: "appended_after_newline",
      draft: "你好\n\n第二段",
      sentText: "你好",
      want: "第二段",
    },
    {
      name: "appended_after_trailing_newline",
      draft: "你好\n第二段",
      sentText: "你好\n",
      want: "第二段",
    },
    {
      name: "rewritten_draft_stays",
      draft: "换了一句",
      sentText: "你好",
      want: "换了一句",
    },
    {
      name: "sent_text_edited_stays",
      draft: "你好吗",
      sentText: "你好呀",
      want: "你好吗",
    },
    {
      name: "image_only_send_keeps_new_text",
      draft: " 新写的",
      sentText: "",
      want: " 新写的",
    },
    { name: "cleared_during_send", draft: "", sentText: "你好", want: "" },
  ]
  for (const each of cases)
    assert.equal(
      removeSentText(each.draft, each.sentText),
      each.want,
      each.name
    )
})

// 混合内容的视图、编辑与发送清理使用同一份 Content。
const material: Content = {
  kind: "text",
  text: { text: "/do-not-run attached", is_context: true },
}
const image: Content = {
  kind: "image",
  image: { data_url: "data:image/png;base64,YQ==", alt: "original alt" },
}
const first: Content = { kind: "text", text: { text: " first " } }
const second: Content = { kind: "text", text: { text: "\nlast  " } }
const mixed = [image, first, material, second]

test("full Content draft preserves order until the ordinary text is edited", () => {
  const cases = [
    {
      name: "ordinary_view_excludes_material",
      run: () => getDraftText(mixed),
      want: " first \nlast  ",
    },
    {
      name: "edit_at_first_text",
      run: () => replaceDraftText(mixed, "edited"),
      want: [image, { kind: "text", text: { text: "edited" } }, material],
    },
    {
      name: "no_text_insert_at_start",
      run: () => replaceDraftText([image, material], "new"),
      want: [{ kind: "text", text: { text: "new" } }, image, material],
    },
    {
      name: "clear_only_ordinary",
      run: () => replaceDraftText(mixed, ""),
      want: [image, material],
    },
  ]
  for (const each of cases) assert.deepEqual(each.run(), each.want, each.name)
  setDraft("server-1", "mixed", { content: mixed })
  assert.equal(getDraft("server-1", "mixed").content, mixed)
  assert.deepEqual(getDraft("server-1", "mixed").content, [
    image,
    first,
    material,
    second,
  ])
})

test("successful send removes only the sent Content", () => {
  const extra: Content = {
    kind: "image",
    image: { data_url: "data:image/png;base64,Yg==", alt: "new" },
  }
  const cases = [
    { name: "unchanged", draft: { content: mixed }, sent: mixed, want: [] },
    {
      name: "new_image",
      draft: { content: [...mixed, extra] },
      sent: mixed,
      want: [extra],
    },
    {
      name: "edited_and_appended",
      draft: {
        content: [
          ...replaceDraftText(mixed, " first \nlast  continued"),
          extra,
        ],
      },
      sent: mixed,
      want: [{ kind: "text", text: { text: "continued" } }, extra],
    },
    {
      name: "rewritten",
      draft: { content: [...replaceDraftText(mixed, "replacement"), extra] },
      sent: mixed,
      want: [{ kind: "text", text: { text: "replacement" } }, extra],
    },
    {
      name: "image_only_keeps_new_text",
      draft: { content: [first, image] },
      sent: [image],
      want: [first],
    },
    {
      name: "failed_send_not_cleared",
      draft: { content: mixed },
      sent: [],
      want: mixed,
    },
  ]
  for (const each of cases)
    assert.deepEqual(
      removeSentContent(each.draft, each.sent).content,
      each.want,
      each.name
    )
})
