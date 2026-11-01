import assert from "node:assert/strict"
import { test } from "node:test"
import { cn } from "./utils"

test("文字角色合并保留主题颜色，并由最后的角色覆盖字号", () => {
  const cases = [
    {
      classes: ["text-xs text-primary-foreground", "text-ui"],
      expected: "text-primary-foreground text-ui",
    },
    {
      classes: ["text-ui text-muted-foreground", "text-caption"],
      expected: "text-muted-foreground text-caption",
    },
    {
      classes: ["text-table text-help text-foreground", "text-token"],
      expected: "text-help text-foreground text-token",
    },
    {
      classes: ["text-token", "text-code text-destructive"],
      expected: "text-code text-destructive",
    },
    {
      classes: ["text-ui color-muted", "color-subtle"],
      expected: "text-ui color-subtle",
    },
    {
      classes: ["color-subtle text-ui", "text-foreground"],
      expected: "text-ui text-foreground",
    },
    {
      classes: ["text-muted-foreground text-caption", "color-subtle"],
      expected: "text-caption color-subtle",
    },
    {
      classes: [
        "color-subtle hover:color-muted text-ui",
        "hover:color-default focus-visible:color-default",
      ],
      expected:
        "color-subtle text-ui hover:color-default focus-visible:color-default",
    },
    {
      classes: [
        "pr-8 group-has-data-[sidebar=menu-action]/menu-item:pr-8",
        "group-has-data-[sidebar=menu-action]/menu-item:pr-10",
      ],
      expected: "pr-8 group-has-data-[sidebar=menu-action]/menu-item:pr-10",
    },
  ]
  for (const { classes, expected } of cases)
    assert.equal(cn(...classes), expected, classes.join(" + "))
})
