# 作物模型辨识度重做实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 以精致低多边形风格重做 14 种问题作物，使同组作物具有可辨识的整株轮廓，并修复香蕉空间关系与灵芝阶段缺失。

**Architecture:** 保持 `createCropModel(def, opts)` 公共接口不变，在 `crops.js` 内新增可复用植物学零件和按作物分派的专用构建函数。关键结构使用稳定对象名称，`crops.visualStructure.test.js` 通过包围盒、对象名称、材质颜色和数量验证模型结构；最终以模型工坊全阶段实拍做视觉验收。

**Tech Stack:** JavaScript ES Modules、Three.js 0.170、Node.js `node:test`、Vite 6、模型工坊 WebGL 页面。

## Global Constraints

- 保持精致低多边形风格，不引入新依赖。
- 保持 `createCropModel(def, opts)`、`mat()`、`isSharedMaterial()` 的外部签名与共享材质语义。
- 核心结构不得使用 `Math.random()`；同一作物同一阶段每次构建应有稳定轮廓。
- 单株任一阶段不得超过 160 个 Mesh。
- 生成配置 `client/src/game/gen/crops.js` 不直接修改。
- 不修改本次任务之外的现有未提交文件。
- 未经用户明确要求不创建 Git 提交。

---

### Task 1: 建立视觉结构回归测试

**Files:**
- Create: `client/src/game/crops.visualStructure.test.js`
- Read: `client/src/game/crops.js`
- Read: `client/src/game/config.js`

**Interfaces:**
- Consumes: `createCropModel(def, { stage, totalStages, mature, withered })`
- Produces: 目标模型的结构契约和测试辅助函数 `build(id, stageOrMature)`、`boundsOf(object)`、`meshCount(object)`

- [ ] **Step 1: 写目标作物全阶段基本结构测试**

测试遍历 `qiezi`、`fanqie`、`wandou`、`hongmeigui`、`lajiao`、`hongzao`、`pingguo`、`caomei`、`xiangjiao`、`chengzi`、`youzi`、`boluo`、`renshen`、`lingzhi`，要求各阶段 Mesh 数大于 0、包围盒尺寸有限、单阶段 Mesh 数不超过 160。

- [ ] **Step 2: 写当前实现必然失败的辨识度测试**

加入以下断言：

```js
assert.ok(model.getObjectByName('eggplant-plant'))
assert.ok(model.getObjectByName('tomato-fruit-cluster'))
assert.ok(model.getObjectByName('pea-vines'))
assert.ok(model.getObjectByName('pepper-plant'))
assert.ok(model.getObjectByName('rose-branches'))
assert.ok(model.getObjectByName('banana-canopy'))
assert.ok(model.getObjectByName('banana-bunch'))
assert.ok(model.getObjectByName('ginseng-root'))
assert.ok(model.getObjectByName('ginseng-compound-leaves'))
assert.ok(model.getObjectByName('lingzhi-cap'))
```

另验证：

```js
assert.ok(appleSize.x / appleSize.y > jujubeSize.x / jujubeSize.y)
assert.ok(jujube.getObjectsByProperty('name', 'jujube-fruit').length >
  apple.getObjectsByProperty('name', 'apple-fruit').length)
assert.ok(pomeloSize.y > orangeSize.y)
assert.equal(strawberryFruit.material.color.getHex(), 0xd9364b)
assert.ok(bananaBunchCenter.y < bananaCanopyCenter.y)
```

- [ ] **Step 3: 运行新测试并确认按预期失败**

Run: `cd client && node --test src/game/crops.visualStructure.test.js`

Expected: FAIL，原因是语义对象尚不存在、草莓颜色仍为 `0xff4d6d`、灵芝中期无 `lingzhi-cap`，而不是语法或导入错误。

---

### Task 2: 增加专用植物零件和阶段上下文

**Files:**
- Modify: `client/src/game/crops.js:40-130`
- Modify: `client/src/game/crops.js:925-970`
- Test: `client/src/game/crops.visualStructure.test.js`

**Interfaces:**
- Produces:
  - `branchBetween(length, radius, color)`：低面枝条
  - `compoundLeaflet(...)`：复叶组合
  - `lanceLeaf(...)`：狭长叶
  - `kidneyCap(width, depth, color, rimColor)`：肾形灵芝菌盖
  - 构建器第三参数 `context`：`{ young, stage, totalStages, mature }`

- [ ] **Step 1: 将内部构建器参数从布尔 `young` 改为上下文对象**

`createCropModel` 仍接收原参数，但调用：

```js
const context = {
  young: !opts.mature && opts.stage < opts.totalStages - 1,
  stage: opts.stage,
  totalStages: opts.totalStages,
  mature: Boolean(opts.mature),
}
const body = buildSafe(def, fruits, context)
```

`buildSafe` 默认上下文必须完整，枯萎路径传入成熟上下文，避免构建器读取 `undefined`。

- [ ] **Step 2: 实现低面植物学辅助件**

辅助件只组合现有 `sphere`、`cone`、`cyl`、`leaf`；每个辅助件返回 `THREE.Group`，不创建独占材质。弯曲器官使用 2–3 段圆柱或圆锥表达，不引入曲线挤出几何。

- [ ] **Step 3: 运行共享材质和新结构测试**

Run: `cd client && node --test src/game/crops.sharedMat.test.js src/game/crops.visualStructure.test.js`

Expected: 共享材质测试 PASS；结构测试仍因专用模型尚未实现而 FAIL。

---

### Task 3: 重做四种灌木与红玫瑰

**Files:**
- Modify: `client/src/game/crops.js:465-573`
- Modify: `client/src/game/crops.js:199-281`
- Test: `client/src/game/crops.visualStructure.test.js`

**Interfaces:**
- Produces:
  - `buildEggplant(def, fruits, context)`
  - `buildTomato(def, fruits, context)`
  - `buildPea(def, fruits, context)`
  - `buildPepper(def, fruits, context)`
  - `buildRose(def, fruits, context)`

- [ ] **Step 1: 让 `BUILDERS.bush` 仅按 ID 分派**

土豆保留当前土垄逻辑；茄子、番茄、豌豆、辣椒进入各自构建函数。不得先创建通用多面体叶球。

- [ ] **Step 2: 实现茄子**

创建命名根组 `eggplant-plant`，紫绿主茎高约 1.05，三条可见分枝；宽叶在分枝端部；成熟时 3 个 `eggplant-fruit` 位于叶冠下方，梨形紫果与绿色星形萼片同组。

- [ ] **Step 3: 实现番茄**

创建 `tomato-plant` 和 2 个 `tomato-fruit-cluster`；斜枝展开宽度大于高度的 60%，每个果簇 3–4 果；复叶由多个小叶组成。开花阶段的黄花锚定到相同果簇位置。

- [ ] **Step 4: 实现豌豆**

创建 `pea-vines`；两根攀援藤、交替复叶和卷须构成长而窄的轮廓。成熟期至少 4 个 `pea-pod` 成对沿藤下垂，豆荚内的豆粒凸起不脱离荚体。

- [ ] **Step 5: 实现辣椒**

创建 `pepper-plant`；直立主茎、4–5 片狭长叶、4–6 个 `pepper-fruit` 分布在不同高度。果实使用两段几何形成自然弯曲，果顶带萼片。

- [ ] **Step 6: 实现红玫瑰**

创建 `rose-branches`；三根高度不同的枝条，每根有复叶和 1–2 个低面刺。成熟花建立 `rose-bloom`，主花大于侧花，花瓣有内外两层；开花阶段用 `rose-bud`，不提前显示成熟花。

- [ ] **Step 7: 运行结构测试**

Run: `cd client && node --test src/game/crops.visualStructure.test.js`

Expected: 灌木与玫瑰相关断言 PASS；其余未实现断言仍 FAIL。

---

### Task 4: 拉开苹果/红枣、橙子/柚子并校正草莓

**Files:**
- Modify: `client/src/game/crops.js:637-697`
- Modify: `client/src/game/crops.js:855-923`
- Test: `client/src/game/crops.visualStructure.test.js`

**Interfaces:**
- Produces: `apple-canopy`、`apple-fruit`、`jujube-canopy`、`jujube-fruit`、`orange-canopy`、`orange-fruit`、`pomelo-canopy`、`pomelo-fruit`、`strawberry-fruit`

- [ ] **Step 1: 重做苹果和红枣树冠构图**

苹果使用低干、三条横向粗枝和 4 组重叠宽冠；红枣使用高细干、4 层较小且错开的冠层并显露细枝。不要只调整 `TREE_STYLE.c1`。

- [ ] **Step 2: 重做橙子和柚子树冠构图**

橙子保持紧凑近球形；柚子使用更高主干、开放式三主枝和较长叶片装饰。柚果组命名 `pomelo-fruit`，果实梨形、浅黄绿色、长梗、数量少于橙子。

- [ ] **Step 3: 校正草莓颜色和叶形**

成熟果体与果尖统一使用 `0xd9364b`，少量次熟果使用固定较浅红色；果实组命名 `strawberry-fruit`。叶丛改为三出复叶组，颜色比当前 `def.leaf` 深一档，保留暖黄籽点。

- [ ] **Step 4: 运行结构测试**

Run: `cd client && node --test src/game/crops.visualStructure.test.js`

Expected: 苹果/红枣、橙子/柚子、草莓相关断言 PASS。

---

### Task 5: 拆分香蕉并重做菠萝

**Files:**
- Modify: `client/src/game/crops.js:748-819`
- Test: `client/src/game/crops.visualStructure.test.js`

**Interfaces:**
- Produces: `banana-canopy`、`banana-bunch`、`pineapple-rosette`、`pineapple-fruit-body`、`pineapple-eye`、`pineapple-crown`

- [ ] **Step 1: 将香蕉从通用棕榈构图中拆出**

香蕉使用短粗假茎和 6 片宽叶；椰子保留原棕榈轮廓。`banana-canopy` 的中心位于果串上方，叶片从冠心向外扇开。

- [ ] **Step 2: 重做蕉串**

果轴从冠心向下延伸，`banana-bunch` 有 2–3 层，每层 4–5 根果指。每根果指用两段黄色几何形成上弯，蕉串整体不得穿过树干或高于叶冠中心。

- [ ] **Step 3: 重做菠萝莲座与果眼**

`pineapple-rosette` 使用外平内直的两层长硬叶。`pineapple-fruit-body` 与莲座中心相接；`pineapple-eye` 为贴合果面的扁菱形片，径向突出不超过 0.035；`pineapple-crown` 高度不超过果身高度 45%。

- [ ] **Step 4: 运行结构测试**

Run: `cd client && node --test src/game/crops.visualStructure.test.js`

Expected: 香蕉与菠萝相关断言 PASS。

---

### Task 6: 重做人参与灵芝完整阶段

**Files:**
- Modify: `client/src/game/crops.js:130-186`
- Modify: `client/src/game/crops.js:300-355`
- Modify: `client/src/game/crops.js:821-833`
- Test: `client/src/game/crops.visualStructure.test.js`

**Interfaces:**
- Produces: `ginseng-root`、`ginseng-compound-leaves`、`ginseng-berries`、各阶段 `lingzhi-cap`

- [ ] **Step 1: 实现人参专用幼苗和成株**

`sprout` 对 `renshen` 使用细茎和三枚小复叶。成株 `ginseng-root` 由主根、两条叉根、两条侧根和细须根组成；`ginseng-compound-leaves` 在顶端形成掌状轮廓；成熟 `ginseng-berries` 为伞形红果簇。

- [ ] **Step 2: 实现灵芝阶段构建器**

根据 `context.stage` 和 `context.mature` 直接构建菌蕾、圆肾小盖、单层扇面、带环展开扇面或成熟双扇面。阶段 1 以后每个模型都必须包含 `lingzhi-cap`；菌盖属于主体，不放入仅成熟期显示的 `fruits`。

- [ ] **Step 3: 禁止灵芝进入通用开花叠加**

`flowerShow` 对 `fungus` 返回 `null`，灵芝开花阶段的视觉变化完全由菌盖展开表达。

- [ ] **Step 4: 运行新结构测试并确认全部通过**

Run: `cd client && node --test src/game/crops.visualStructure.test.js`

Expected: PASS，目标结构测试零失败。

---

### Task 7: 全量回归与视觉审核

**Files:**
- Modify only if review finds defects: `client/src/game/crops.js`
- Test: `client/src/game/crops.visualStructure.test.js`
- Verify: `client/model-workshop.html`

**Interfaces:**
- Consumes: 完成后的 `createCropModel`
- Produces: 全阶段对照图和最终审核结论

- [ ] **Step 1: 运行客户端全部测试**

Run: `cd client && node --test src/game/*.test.js src/*.test.js`

Expected: exit code 0，零失败。

- [ ] **Step 2: 运行生产构建**

Run: `cd client && npm run build`

Expected: exit code 0，无 Vite 构建错误。

- [ ] **Step 3: 运行 29 种作物全阶段无头冒烟测试**

对 `CROPS` 每项调用全部阶段与枯萎阶段，遍历 Mesh，验证包围盒有限、Mesh 数在 `(0, 160]`；Expected: 失败数 0。

- [ ] **Step 4: 在模型工坊导出目标作物全阶段对照图**

按行排列 14 种目标作物，按列排列发芽、小叶、大叶、开花、成熟；每格等待标题匹配并至少等待 5 个 `requestAnimationFrame` 后截图。

- [ ] **Step 5: 审核并形成最多一轮明确调整清单**

逐项检查同组剪影、果实挂点、颜色、穿插、阶段连续性和默认相机取景。若有缺陷，将“作物 + 阶段 + 可观察问题 + 精确改法”反馈给实现子代理；调整后重新执行 Step 1–4。

## 计划自检

- 设计中的 14 种目标作物均有对应实施步骤。
- 灵芝阶段错误、香蕉空间关系、相似作物辨识度和草莓颜色均有自动化断言。
- 计划不修改生成配置、不改变公共接口、不引入依赖。
- 所有生产改动之前都有会失败的结构测试。
- 不包含未定义的占位任务或未经授权的提交步骤。
