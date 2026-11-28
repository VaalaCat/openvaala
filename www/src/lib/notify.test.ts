import assert from "node:assert/strict"
import { test } from "node:test"
import { toast } from "sonner"
import { notifySuccess } from "./notify"

test("success toast preserves descriptions and passes through the action", (t) => {
  const success = t.mock.method(toast, "success", () => 1)
  const onClick = t.mock.fn()
  notifySuccess("Forwarded", { label: "Open contacts", onClick })
  notifySuccess("Updated", "Reload pending")
  notifySuccess("Saved")

  const [withAction, withDescription, plain] = success.mock.calls.map(
    (call) => call.arguments
  )
  assert.deepEqual(withAction, [
    "Forwarded",
    { duration: 4000, action: { label: "Open contacts", onClick } },
  ])
  assert.deepEqual(withDescription, [
    "Updated",
    { duration: 4000, description: "Reload pending" },
  ])
  assert.deepEqual(plain, ["Saved", { duration: 4000 }])
})
