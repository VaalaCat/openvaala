import assert from "node:assert/strict"
import { test } from "node:test"
import { reconcileRotatingCharacters } from "./rotating-text-characters"

test("逐字旋转只替换变化字素并保留共同文字的标识", async (t) => {
  const cases = [
    {
      name: "次数变化",
      before: "思考1次",
      after: "思考2次",
      entering: "2",
      exiting: "1",
      retained: [0, 1, null, 3],
    },
    {
      name: "前置工具次数",
      before: "思考2次",
      after: "执行1次工具，思考2次",
      entering: "执行1次工具，",
      exiting: "",
      retained: [null, null, null, null, null, null, null, 0, 1, 2, 3],
    },
    {
      name: "动作替换",
      before: "正在读取文件",
      after: "正在执行命令",
      entering: "执行命令",
      exiting: "读取文件",
      retained: [0, 1, null, null, null, null],
    },
    {
      name: "文件名替换",
      before: "读取a.ts",
      after: "读取b.ts",
      entering: "b",
      exiting: "a",
      retained: [0, 1, null, 3, 4, 5],
    },
    {
      name: "重复字符按位置复用",
      before: "aabaa",
      after: "aacaa",
      entering: "c",
      exiting: "b",
      retained: [0, 1, null, 3, 4],
    },
    {
      name: "重复片段中间插入",
      before: "abcabc",
      after: "abcXabc",
      entering: "X",
      exiting: "",
      retained: [0, 1, 2, null, 3, 4, 5],
    },
    {
      name: "emoji和组合字符",
      before: "读👩‍💻é🇨🇳",
      after: "读👨‍💻é🇨🇳",
      entering: "👨‍💻",
      exiting: "👩‍💻",
      retained: [0, null, 2, 3],
    },
    {
      name: "尾部追加",
      before: "正在读取",
      after: "正在读取文件",
      entering: "文件",
      exiting: "",
      retained: [0, 1, 2, 3, null, null],
    },
    {
      name: "清空",
      before: "读取",
      after: "",
      entering: "",
      exiting: "读取",
      retained: [],
    },
  ]
  for (const scenario of cases) {
    await t.test(scenario.name, () => {
      const initial = reconcileRotatingCharacters([], scenario.before, 0)
      const result = reconcileRotatingCharacters(
        initial.characters,
        scenario.after,
        initial.nextID
      )
      assert.equal(
        result.characters.map(({ value }) => value).join(""),
        scenario.after
      )
      assert.equal(
        result.entering.map(({ value }) => value).join(""),
        scenario.entering
      )
      assert.equal(
        result.exiting.map(({ value }) => value).join(""),
        scenario.exiting
      )
      assert.equal(
        new Set(result.characters.map(({ id }) => id)).size,
        result.characters.length
      )
      scenario.retained.forEach((id, index) => {
        if (id === null)
          assert.ok(result.characters[index].id >= initial.nextID)
        else
          assert.strictEqual(result.characters[index], initial.characters[id])
      })
    })
  }
})

test("流式追加和连续替换使用最新字素并且不复用已退出标识", async (t) => {
  for (const scenario of [
    { texts: ["读", "读取", "读取文件"], append: [true, true] },
    { texts: ["读取a.ts", "读取b.ts", "读取a.ts"], append: [false, false] },
    { texts: ["e", "é", "é👩‍💻"], append: [false, true] },
  ]) {
    await t.test(scenario.texts.join(" → "), () => {
      let result = reconcileRotatingCharacters([], scenario.texts[0], 0)
      const retiredIDs = new Set<number>()
      scenario.texts.slice(1).forEach((text, index) => {
        result = reconcileRotatingCharacters(
          result.characters,
          text,
          result.nextID
        )
        assert.equal(result.isAppend, scenario.append[index])
        result.exiting.forEach(({ id }) => retiredIDs.add(id))
        assert.ok(result.characters.every(({ id }) => !retiredIDs.has(id)))
        assert.equal(result.characters.map(({ value }) => value).join(""), text)
      })
    })
  }
})

test("长摘要限制视觉字素数量并保留完整字素与共同前后缀", async (t) => {
  for (const scenario of [
    {
      name: "恰好160字不省略",
      text: "读".repeat(160),
      visible: "读".repeat(160),
    },
    {
      name: "超出一字省略",
      text: "读".repeat(161),
      visible: "读".repeat(159) + "…",
    },
    {
      name: "十万字摘要",
      text: "读".repeat(100_000),
      visible: "读".repeat(159) + "…",
    },
    {
      name: "emoji不拆分",
      text: "👩‍💻".repeat(10_000),
      visible: "👩‍💻".repeat(159) + "…",
    },
    {
      name: "组合字不拆分",
      text: "é".repeat(10_000),
      visible: "é".repeat(159) + "…",
    },
  ]) {
    await t.test(scenario.name, () => {
      const result = reconcileRotatingCharacters([], scenario.text, 0)
      assert.equal(result.characters.length, 160)
      assert.equal(
        result.characters.map(({ value }) => value).join(""),
        scenario.visible
      )
    })
  }
  for (const length of [150, 5_000, 100_000]) {
    await t.test(`改写${length}字仍复用共同首尾`, () => {
      const initial = reconcileRotatingCharacters(
        [],
        "首" + "a".repeat(length) + "尾",
        0
      )
      const result = reconcileRotatingCharacters(
        initial.characters,
        "首" + "b".repeat(length) + "尾",
        initial.nextID
      )
      assert.ok(result.characters.length <= 160)
      assert.strictEqual(result.characters[0], initial.characters[0])
      assert.strictEqual(result.characters.at(-1), initial.characters.at(-1))
      assert.equal(
        new Set(result.characters.map(({ id }) => id)).size,
        result.characters.length
      )
    })
  }
  for (const length of [130, 145, 160]) {
    await t.test(`${length}字摘要首尾改写时复用中间共同片段`, () => {
      const middle = "读".repeat(length - 2)
      const initial = reconcileRotatingCharacters([], `甲${middle}乙`, 0)
      const result = reconcileRotatingCharacters(
        initial.characters,
        `丙${middle}丁`,
        initial.nextID
      )
      assert.equal(result.characters.length, length)
      assert.equal(result.entering.map(({ value }) => value).join(""), "丙丁")
      assert.equal(result.exiting.map(({ value }) => value).join(""), "甲乙")
      for (let index = 1; index < length - 1; index++) {
        assert.strictEqual(result.characters[index], initial.characters[index])
      }
    })
  }
  await t.test("省略范围外追加不重播可见文字", () => {
    const initial = reconcileRotatingCharacters([], "a".repeat(5_000), 0)
    const result = reconcileRotatingCharacters(
      initial.characters,
      "a".repeat(10_000),
      initial.nextID
    )
    assert.equal(result.isAppend, true)
    assert.deepEqual(result.entering, [])
    assert.deepEqual(result.exiting, [])
    result.characters.forEach((character, index) =>
      assert.strictEqual(character, initial.characters[index])
    )
  })
})
