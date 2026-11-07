import assert from "node:assert/strict"
import { test } from "node:test"
import type { Conversation, ExecutorSettings } from "./api/protocol.gen"
import {
  getConversationWorkLocation,
  getRecentDirectories,
  getWorkLocationSettings,
} from "./work-location"

function createConversation(executorSettings: ExecutorSettings): Conversation {
  return {
    id: "conversation-1",
    executor_settings: executorSettings,
    root_session_id: "session-1",
  }
}

test("conversation work location falls back to local and drops other executors", () => {
  const cases: {
    name: string
    settings: ExecutorSettings
    wantLocation: { executorID: string; directory: string }
    wantSettings: ExecutorSettings
  }[] = [
    {
      name: "default executor with directory",
      settings: {
        default_executor_id: "ssh",
        executor_ids: ["ssh", "local"],
        workspace_directories: { ssh: "/srv/app", local: "/home/app" },
      },
      wantLocation: { executorID: "ssh", directory: "/srv/app" },
      wantSettings: {
        default_executor_id: "ssh",
        workspace_directories: { ssh: "/srv/app" },
      },
    },
    {
      name: "empty default executor is local",
      settings: { workspace_directories: { local: "/home/app", ssh: "/srv" } },
      wantLocation: { executorID: "local", directory: "/home/app" },
      wantSettings: {
        default_executor_id: "local",
        workspace_directories: { local: "/home/app" },
      },
    },
    {
      name: "default executor without saved directory",
      settings: {
        default_executor_id: "ssh",
        workspace_directories: { local: "/home/app" },
      },
      wantLocation: { executorID: "ssh", directory: "" },
      wantSettings: {
        default_executor_id: "ssh",
        workspace_directories: undefined,
      },
    },
  ]
  for (const item of cases) {
    const conversation = createConversation(item.settings)
    assert.deepEqual(
      getConversationWorkLocation(conversation),
      item.wantLocation,
      item.name
    )
    assert.deepEqual(
      getWorkLocationSettings(conversation),
      item.wantSettings,
      item.name
    )
  }
})

test("recent directories keep the newest order for the chosen executor", () => {
  const conversations = [
    createConversation({ workspace_directories: { local: "/a", ssh: "/s0" } }),
    createConversation({ workspace_directories: { local: "/b" } }),
    createConversation({
      default_executor_id: "ssh",
      workspace_directories: { local: "/root", ssh: "/s1" },
    }),
    createConversation({
      default_executor_id: "ssh",
      workspace_directories: { ssh: "/s2" },
    }),
    createConversation({ workspace_directories: { local: "/a" } }),
    createConversation({ workspace_directories: { local: "" } }),
    createConversation({}),
    createConversation({ workspace_directories: { local: "/c" } }),
    createConversation({ workspace_directories: { local: "/d" } }),
  ]
  const cases: {
    name: string
    executorID: string
    limit: number
    want: string[]
  }[] = [
    {
      name: "dedupes and skips empty directories",
      executorID: "local",
      limit: 5,
      want: ["/a", "/b", "/c", "/d"],
    },
    {
      name: "stops at the limit",
      executorID: "local",
      limit: 2,
      want: ["/a", "/b"],
    },
    {
      name: "only lists conversations whose default executor is the chosen one",
      executorID: "ssh",
      limit: 5,
      want: ["/s1", "/s2"],
    },
    {
      name: "executor without directories",
      executorID: "other",
      limit: 5,
      want: [],
    },
  ]
  for (const item of cases)
    assert.deepEqual(
      getRecentDirectories(conversations, item.executorID, item.limit),
      item.want,
      item.name
    )
})
