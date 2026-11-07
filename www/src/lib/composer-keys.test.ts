import assert from "node:assert/strict"
import { test } from "node:test"
import {
  getSideConversationQuestion,
  getSlashToken,
  shouldSubmitOnKeyDown,
} from "./composer-keys"

test("shouldSubmitOnKeyDown sends on a plain Enter only", () => {
  const cases: {
    name: string
    event: Parameters<typeof shouldSubmitOnKeyDown>[0]
    want: boolean
  }[] = [
    {
      name: "enter",
      event: {
        key: "Enter",
        shiftKey: false,
        isComposing: false,
        keyCode: 13,
        isTouchPrimary: false,
      },
      want: true,
    },
    {
      name: "shift_enter_newline",
      event: {
        key: "Enter",
        shiftKey: true,
        isComposing: false,
        keyCode: 13,
        isTouchPrimary: false,
      },
      want: false,
    },
    {
      name: "composing",
      event: {
        key: "Enter",
        shiftKey: false,
        isComposing: true,
        keyCode: 13,
        isTouchPrimary: false,
      },
      want: false,
    },
    {
      name: "safari_ime_keycode_229",
      event: {
        key: "Enter",
        shiftKey: false,
        isComposing: false,
        keyCode: 229,
        isTouchPrimary: false,
      },
      want: false,
    },
    {
      name: "touch_keyboard_enter_newline",
      event: {
        key: "Enter",
        shiftKey: false,
        isComposing: false,
        keyCode: 13,
        isTouchPrimary: true,
      },
      want: false,
    },
    {
      name: "other_key",
      event: {
        key: "a",
        shiftKey: false,
        isComposing: false,
        keyCode: 65,
        isTouchPrimary: false,
      },
      want: false,
    },
  ]
  for (const { name, event, want } of cases)
    assert.equal(shouldSubmitOnKeyDown(event), want, name)
})

test("getSlashToken finds the slash word around the cursor", () => {
  const cases: {
    name: string
    text: string
    cursor: number
    want: ReturnType<typeof getSlashToken>
  }[] = [
    {
      name: "bare_slash",
      text: "/",
      cursor: 1,
      want: { start: 0, end: 1, prefix: "" },
    },
    {
      name: "prefix_typed",
      text: "/rev",
      cursor: 4,
      want: { start: 0, end: 4, prefix: "rev" },
    },
    {
      name: "after_text",
      text: "please /rev now",
      cursor: 11,
      want: { start: 7, end: 11, prefix: "rev" },
    },
    {
      name: "cursor_inside_word",
      text: "/review",
      cursor: 3,
      want: { start: 0, end: 7, prefix: "re" },
    },
    {
      name: "next_line",
      text: "hello\n/sk",
      cursor: 9,
      want: { start: 6, end: 9, prefix: "sk" },
    },
    { name: "plain_word", text: "path", cursor: 4, want: undefined },
    { name: "slash_inside_word", text: "a/b", cursor: 3, want: undefined },
    { name: "cursor_before_slash", text: "/rev", cursor: 0, want: undefined },
    { name: "after_space", text: "/rev ", cursor: 5, want: undefined },
    { name: "empty", text: "", cursor: 0, want: undefined },
  ]
  for (const { name, text, cursor, want } of cases)
    assert.deepEqual(getSlashToken(text, cursor), want, name)
})

test("getSideConversationQuestion only reads a leading /btw command", () => {
  const cases: { name: string; text: string; want: string | undefined }[] = [
    { name: "question", text: "/btw 为什么用 SQLite", want: "为什么用 SQLite" },
    { name: "command_only", text: "/btw", want: "" },
    { name: "blank_after_command", text: "/btw  \n", want: "" },
    {
      name: "multi_line_question",
      text: "/btw 第一行\n第二行",
      want: "第一行\n第二行",
    },
    { name: "longer_word", text: "/btwx 问题", want: undefined },
    { name: "other_skill", text: "/review 问题", want: undefined },
    { name: "not_at_start", text: "先说 /btw 问题", want: undefined },
    { name: "plain_text", text: "btw 问题", want: undefined },
  ]
  for (const { name, text, want } of cases)
    assert.equal(getSideConversationQuestion(text), want, name)
})
