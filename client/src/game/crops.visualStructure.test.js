// 作物模型视觉结构契约测试（crop-model-distinction-redesign）
// 验证 14 种目标作物的语义对象名称、包围盒、数量、颜色与空间关系。
import test from 'node:test'
import assert from 'node:assert/strict'
import * as THREE from 'three'

import { createCropModel } from './crops.js'
import { CROP_MAP, stageCount } from './config.js'

const TARGET_IDS = [
  'qiezi', 'fanqie', 'wandou', 'hongmeigui', 'lajiao',
  'hongzao', 'pingguo', 'caomei', 'xiangjiao', 'chengzi',
  'youzi', 'boluo', 'renshen', 'lingzhi',
]

function build(id, stageOrMature) {
  const def = CROP_MAP[id]
  assert.ok(def, `未知作物 ${id}`)
  const totalStages = stageCount(def)
  const mature = stageOrMature === 'mature'
  const stage = mature ? totalStages : stageOrMature
  return createCropModel(def, { stage, totalStages, mature, withered: false })
}

function boundsOf(object) {
  const box = new THREE.Box3().setFromObject(object)
  const size = box.getSize(new THREE.Vector3())
  const center = box.getCenter(new THREE.Vector3())
  return { box, size, center }
}

function meshCount(object) {
  let n = 0
  object.traverse((o) => { if (o.isMesh) n += 1 })
  return n
}

function named(object, name) {
  return object.getObjectsByProperty('name', name)
}

function meshesWithColor(root, hex) {
  const out = []
  root.traverse((o) => {
    if (o.isMesh && o.material && o.material.color && o.material.color.getHex() === hex) out.push(o)
  })
  return out
}

function worldPos(object) {
  return object.getWorldPosition(new THREE.Vector3())
}

// ---------- Task 1 Step 1: 全阶段基本结构 ----------

test('14 种目标作物全阶段可构建：Mesh 数在 (0,160]，包围盒有限且非零', () => {
  for (const id of TARGET_IDS) {
    const total = stageCount(CROP_MAP[id])
    const stages = [...Array(total).keys(), 'mature']
    for (const st of stages) {
      const model = build(id, st)
      const n = meshCount(model)
      const { size } = boundsOf(model)
      assert.ok(n > 0, `${id} 阶段 ${st} Mesh 数应大于 0`)
      assert.ok(n <= 160, `${id} 阶段 ${st} Mesh 数 ${n} 超过 160`)
      for (const k of ['x', 'y', 'z']) {
        assert.ok(Number.isFinite(size[k]), `${id} 阶段 ${st} 包围盒 ${k} 应有限`)
        assert.ok(size[k] > 0, `${id} 阶段 ${st} 包围盒 ${k} 应大于 0`)
      }
    }
  }
})

// ---------- Task 3: 四种灌木与红玫瑰 ----------

test('茄子：eggplant-plant 三分枝宽叶，eggplant-fruit 悬于叶冠下方带星形萼', () => {
  const def = CROP_MAP.qiezi
  const model = build('qiezi', 'mature')
  const plant = model.getObjectByName('eggplant-plant')
  assert.ok(plant, '缺少 eggplant-plant')
  const leaves = named(model, 'eggplant-leaf')
  assert.ok(leaves.length >= 3, `茄子宽叶应不少于 3 片，实际 ${leaves.length}`)
  const fruits = named(model, 'eggplant-fruit')
  assert.ok(fruits.length >= 2 && fruits.length <= 3, `茄子果应 2-3 个，实际 ${fruits.length}`)
  const leafYs = leaves.map((l) => worldPos(l).y)
  for (const f of fruits) {
    const fb = boundsOf(f)
    assert.ok(fb.center.y < Math.min(...leafYs), '果实应位于叶冠下方')
    assert.ok(meshesWithColor(f, def.fruit).length >= 1, '茄果应为紫色果身')
    assert.ok(meshesWithColor(f, 0x6fae54).length >= 1, '茄果顶部应有绿色星形萼')
  }
  // 主茎高挑：整株明显高大于宽
  const pb = boundsOf(plant)
  assert.ok(pb.size.y > 0.9, `茄子主茎应高挑，实际高 ${pb.size.y.toFixed(2)}`)
})

test('番茄：tomato-plant 斜枝复叶，tomato-fruit-cluster 每簇 3-4 果', () => {
  const def = CROP_MAP.fanqie
  const model = build('fanqie', 'mature')
  const plant = model.getObjectByName('tomato-plant')
  assert.ok(plant, '缺少 tomato-plant')
  const pb = boundsOf(plant)
  assert.ok(
    Math.max(pb.size.x, pb.size.z) > pb.size.y * 0.6,
    `番茄斜枝展开宽度应大于高度 60%，实际 ${pb.size.x.toFixed(2)}/${pb.size.z.toFixed(2)}/${pb.size.y.toFixed(2)}`,
  )
  const clusters = named(model, 'tomato-fruit-cluster')
  assert.ok(clusters.length >= 2, `番茄果簇应不少于 2 个，实际 ${clusters.length}`)
  for (const c of clusters) {
    const reds = meshesWithColor(c, def.fruit)
    assert.ok(reds.length >= 3 && reds.length <= 4, `每簇番茄应 3-4 颗，实际 ${reds.length}`)
  }
})

test('番茄开花阶段：黄色花簇锚定在与成熟果簇相同的位置', () => {
  const matureModel = build('fanqie', 'mature')
  const clusterCenters = named(matureModel, 'tomato-fruit-cluster').map((c) => boundsOf(c).center)
  assert.ok(clusterCenters.length >= 2)
  const flowerModel = build('fanqie', stageCount(CROP_MAP.fanqie) - 1)
  const yellows = meshesWithColor(flowerModel, 0xffd94d).map((m) => worldPos(m))
  assert.ok(yellows.length >= 4, '开花阶段应有黄色星形花')
  for (const cc of clusterCenters) {
    const anchor = cc.clone().multiplyScalar(0.92) // 开花阶段整株缩放 0.92
    const near = yellows.some((p) => p.distanceTo(anchor) < 0.35)
    assert.ok(near, `果簇位置 (${anchor.x.toFixed(2)},${anchor.y.toFixed(2)},${anchor.z.toFixed(2)}) 附近应有黄花`)
  }
})

test('豌豆：pea-vines 攀援细高轮廓，pea-pod 成对下垂不少于 4 个', () => {
  const model = build('wandou', 'mature')
  const vines = model.getObjectByName('pea-vines')
  assert.ok(vines, '缺少 pea-vines')
  const vb = boundsOf(vines)
  assert.ok(vb.size.y > Math.max(vb.size.x, vb.size.z), '豌豆应呈细高攀援轮廓')
  const pods = named(model, 'pea-pod')
  assert.ok(pods.length >= 4, `豌豆荚应不少于 4 个，实际 ${pods.length}`)
  const centers = pods.map((p) => boundsOf(p).center)
  let minPair = Infinity
  for (let i = 0; i < centers.length; i++) {
    for (let j = i + 1; j < centers.length; j++) {
      minPair = Math.min(minPair, centers[i].distanceTo(centers[j]))
    }
  }
  assert.ok(minPair < 0.2, `豆荚应成对生长，最近对距 ${minPair.toFixed(2)}`)
  for (const p of pods) {
    const pb = boundsOf(p)
    assert.ok(pb.size.y > pb.size.x && pb.size.y > pb.size.z, '豆荚应下垂呈竖长形')
  }
})

test('辣椒：pepper-plant 直立披针叶，4-6 个 pepper-fruit 分高度下垂带萼', () => {
  const def = CROP_MAP.lajiao
  const model = build('lajiao', 'mature')
  const plant = model.getObjectByName('pepper-plant')
  assert.ok(plant, '缺少 pepper-plant')
  const pb = boundsOf(plant)
  assert.ok(pb.size.y > Math.max(pb.size.x, pb.size.z) * 0.9, '辣椒应呈直立细株轮廓')
  const fruits = named(model, 'pepper-fruit')
  assert.ok(fruits.length >= 4 && fruits.length <= 6, `辣椒果应 4-6 个，实际 ${fruits.length}`)
  const heights = new Set(fruits.map((f) => Math.round(boundsOf(f).center.y * 10)))
  assert.ok(heights.size >= 3, '辣椒果应分布在不同高度')
  for (const f of fruits) {
    assert.ok(meshesWithColor(f, def.fruit).length >= 1, '椒果应为红色')
    assert.ok(meshesWithColor(f, 0x5aa54a).length >= 1, '椒果顶部应有绿色萼片')
  }
})

test('四种灌木成熟轮廓互不相同（语义名称 + 包围盒）', () => {
  const signatures = new Map()
  for (const id of ['qiezi', 'fanqie', 'wandou', 'lajiao']) {
    const { size } = boundsOf(build(id, 'mature'))
    const sig = [size.x, size.y, size.z].map((v) => v.toFixed(1)).join('x')
    assert.ok(!signatures.has(sig), `${id} 与 ${signatures.get(sig)} 成熟包围盒过于接近`)
    signatures.set(sig, id)
  }
})

test('红玫瑰：rose-branches 错落木枝，成熟 1 主花 2 侧花，开花期为花苞', () => {
  const matureModel = build('hongmeigui', 'mature')
  const branches = matureModel.getObjectByName('rose-branches')
  assert.ok(branches, '缺少 rose-branches')
  assert.ok(meshesWithColor(branches, 0x7a5a44).length >= 3, '玫瑰应有 3 根木质化枝条')
  const blooms = named(matureModel, 'rose-bloom')
  assert.equal(blooms.length, 3, `成熟玫瑰应为 1 主花 + 2 侧花，实际 ${blooms.length}`)
  const sizes = blooms.map((b) => boundsOf(b).size.y)
  assert.ok(Math.max(...sizes) > Math.min(...sizes) * 1.15, '主花应明显大于侧花')
  const heights = new Set(blooms.map((b) => Math.round(boundsOf(b).center.y * 10)))
  assert.ok(heights.size >= 2, '花朵应位于错落高度')

  const budModel = build('hongmeigui', stageCount(CROP_MAP.hongmeigui) - 1)
  const buds = named(budModel, 'rose-bud')
  assert.ok(buds.length >= 3, `开花期应有不少于 3 个花苞，实际 ${buds.length}`)
  assert.equal(named(budModel, 'rose-bloom').length, 0, '开花期不应提前出现成熟花')
  const budSizes = new Set(buds.map((b) => Math.round(boundsOf(b).size.y * 20)))
  assert.ok(budSizes.size >= 2, '花苞应有不同大小')
})

// ---------- Task 4: 苹果/红枣、橙子/柚子、草莓 ----------

test('苹果宽圆矮冠 vs 红枣疏朗高层冠：宽高比与果数显著不同', () => {
  const apple = build('pingguo', 'mature')
  const jujube = build('hongzao', 'mature')
  const appleCanopy = apple.getObjectByName('apple-canopy')
  const jujubeCanopy = jujube.getObjectByName('jujube-canopy')
  assert.ok(appleCanopy, '缺少 apple-canopy')
  assert.ok(jujubeCanopy, '缺少 jujube-canopy')
  const ab = boundsOf(appleCanopy)
  const jb = boundsOf(jujubeCanopy)
  const appleRatio = ab.size.x / ab.size.y
  const jujubeRatio = jb.size.x / jb.size.y
  assert.ok(
    appleRatio > jujubeRatio + 0.3,
    `苹果冠宽高比 ${appleRatio.toFixed(2)} 应明显高于红枣 ${jujubeRatio.toFixed(2)}`,
  )
  const appleFruits = named(apple, 'apple-fruit')
  const jujubeFruits = named(jujube, 'jujube-fruit')
  assert.ok(appleFruits.length >= 6 && appleFruits.length <= 7, `苹果果数应 6-7，实际 ${appleFruits.length}`)
  assert.ok(jujubeFruits.length >= 10 && jujubeFruits.length <= 12, `红枣果数应 10-12，实际 ${jujubeFruits.length}`)
  assert.ok(jujubeFruits.length > appleFruits.length, '红枣命名果实数量应多于苹果')
  // 果实纵横比：苹果微扁，红枣竖长椭圆
  const aRatio = boundsOf(appleFruits[0]).size.y / boundsOf(appleFruits[0]).size.x
  const jRatio = boundsOf(jujubeFruits[0]).size.y / boundsOf(jujubeFruits[0]).size.x
  assert.ok(jRatio > aRatio + 0.15, `红枣果纵比 ${jRatio.toFixed(2)} 应大于苹果 ${aRatio.toFixed(2)}`)
})

test('柚子高于橙子、树冠更疏、果实更大且数量更少', () => {
  const orange = build('chengzi', 'mature')
  const pomelo = build('youzi', 'mature')
  assert.ok(orange.getObjectByName('orange-canopy'), '缺少 orange-canopy')
  assert.ok(pomelo.getObjectByName('pomelo-canopy'), '缺少 pomelo-canopy')
  const ob = boundsOf(orange)
  const pb = boundsOf(pomelo)
  assert.ok(pb.size.y > ob.size.y, `柚子整株高 ${pb.size.y.toFixed(2)} 应高于橙子 ${ob.size.y.toFixed(2)}`)
  const orangeFruits = named(orange, 'orange-fruit')
  const pomeloFruits = named(pomelo, 'pomelo-fruit')
  assert.ok(orangeFruits.length >= 7 && orangeFruits.length <= 8, `橙子果数应 7-8，实际 ${orangeFruits.length}`)
  assert.ok(pomeloFruits.length >= 4 && pomeloFruits.length <= 5, `柚子果数应 4-5，实际 ${pomeloFruits.length}`)
  assert.ok(pomeloFruits.length < orangeFruits.length, '柚子果实数量应少于橙子')
  const ofSize = boundsOf(orangeFruits[0]).size
  const pfSize = boundsOf(pomeloFruits[0]).size
  assert.ok(pfSize.y > ofSize.y * 1.3, `柚子单果高 ${pfSize.y.toFixed(2)} 应明显大于橙子 ${ofSize.y.toFixed(2)}`)
  // 柚子果从较长果梗下垂，果心低于橙子果
  const pomeloFruitY = Math.min(...pomeloFruits.map((f) => boundsOf(f).center.y))
  const pomeloCanopyY = boundsOf(pomelo.getObjectByName('pomelo-canopy')).center.y
  assert.ok(pomeloFruitY < pomeloCanopyY - 0.3, '柚子果应明显垂于树冠之下')
})

test('草莓自然深红果 0xd9364b、暖黄籽点、深绿三出复叶', () => {
  const def = CROP_MAP.caomei
  const model = build('caomei', 'mature')
  const fruits = named(model, 'strawberry-fruit')
  assert.ok(fruits.length >= 4, `草莓果应不少于 4 个，实际 ${fruits.length}`)
  const hasDeepRed = fruits.some((f) => meshesWithColor(f, 0xd9364b).length >= 1)
  assert.ok(hasDeepRed, '草莓果体应使用自然深红 0xd9364b')
  const hasOldPink = fruits.some((f) => meshesWithColor(f, 0xff4d6d).length >= 1)
  assert.ok(!hasOldPink, '不应再使用荧光粉 0xff4d6d')
  const hasSeeds = fruits.some((f) => meshesWithColor(f, 0xffe08a).length >= 1)
  assert.ok(hasSeeds, '草莓籽点应保留暖黄 0xffe08a')
  const leaves = model.getObjectByName('strawberry-leaves')
  assert.ok(leaves, '缺少 strawberry-leaves')
  const leafMeshes = []
  leaves.traverse((o) => { if (o.isMesh) leafMeshes.push(o) })
  assert.ok(leafMeshes.length >= 9, '草莓应为多组三出复叶')
  const leafColor = leafMeshes[0].material.color
  const defColor = new THREE.Color(def.leaf)
  const leafL = leafColor.getHSL({ h: 0, s: 0, l: 0 }).l
  const defL = defColor.getHSL({ h: 0, s: 0, l: 0 }).l
  assert.ok(leafL < defL - 0.03, `草莓叶色应比 def.leaf 深一档（${leafL.toFixed(3)} vs ${defL.toFixed(3)}）`)
})

// ---------- Task 5: 香蕉与菠萝 ----------

test('香蕉 banana-canopy 宽叶扇冠，banana-bunch 多层果串位于叶冠下方', () => {
  const def = CROP_MAP.xiangjiao
  const model = build('xiangjiao', 'mature')
  const canopy = model.getObjectByName('banana-canopy')
  const bunch = model.getObjectByName('banana-bunch')
  assert.ok(canopy, '缺少 banana-canopy')
  assert.ok(bunch, '缺少 banana-bunch')
  const cb = boundsOf(canopy)
  const bb = boundsOf(bunch)
  assert.ok(bb.center.y < cb.center.y, `果串中心 ${bb.center.y.toFixed(2)} 应低于叶冠中心 ${cb.center.y.toFixed(2)}`)
  assert.ok(cb.size.x > 1.6, `蕉叶冠幅应宽大，实际 ${cb.size.x.toFixed(2)}`)
  const fingers = meshesWithColor(bunch, def.fruit)
  assert.ok(fingers.length >= 16, `蕉串应有多层多根果指，实际黄色网格 ${fingers.length}`)
  // 椰子保持原棕榈结构，不出现香蕉部件
  const coconut = build('yezi', 'mature')
  assert.equal(named(coconut, 'banana-canopy').length, 0, '椰子不应有 banana-canopy')
  assert.equal(named(coconut, 'banana-bunch').length, 0, '椰子不应有 banana-bunch')
})

test('菠萝：贴地莲座 + 居中果身 + 贴面果眼 + 矮小冠芽', () => {
  const model = build('boluo', 'mature')
  const rosette = model.getObjectByName('pineapple-rosette')
  const body = model.getObjectByName('pineapple-fruit-body')
  const eyes = named(model, 'pineapple-eye')
  const crown = model.getObjectByName('pineapple-crown')
  assert.ok(rosette, '缺少 pineapple-rosette')
  assert.ok(body, '缺少 pineapple-fruit-body')
  assert.ok(eyes.length >= 20, `果眼应不少于 20 片，实际 ${eyes.length}`)
  assert.ok(crown, '缺少 pineapple-crown')
  assert.ok(meshCount(rosette) >= 10, '莲座应由多片长硬叶组成')
  const bb = boundsOf(body)
  assert.ok(Math.hypot(bb.center.x, bb.center.z) < 0.1, '果身应位于莲座正中心')
  assert.ok(bb.box.min.y < 0.2, `果身底部应与莲座基部相接，实际 ${bb.box.min.y.toFixed(2)}`)
  // 第二轮：果身为饱满金橙，果眼为微凸四棱锥 + 绿色苞片尖（介于削皮平板与朝外尖刺之间）
  assert.ok(meshesWithColor(body, 0xdf9a35).length >= 1, '菠萝果身应使用金橙 0xdf9a35')
  const y0 = bb.box.min.y
  const h = bb.size.y
  for (const eye of eyes) {
    const p = worldPos(eye)
    const t = Math.min(1, Math.max(0, (p.y - y0) / h))
    const surfaceR = 0.3 - 0.06 * t
    const radial = Math.hypot(p.x, p.z)
    assert.ok(radial - surfaceR >= 0.04 - 1e-6, `果眼应微凸果面，实际径向差 ${(radial - surfaceR).toFixed(3)}`)
    assert.ok(radial - surfaceR <= 0.12 + 1e-6, `果眼不可成尖刺，实际径向差 ${(radial - surfaceR).toFixed(3)}`)
    assert.ok(meshesWithColor(eye, 0x5f9e46).length >= 1, '每个果眼顶端应有绿色苞片尖')
  }
  // 冠芽高度不超过果身高度 45%
  const cb = boundsOf(crown)
  assert.ok(cb.size.y <= bb.size.y * 0.45 + 1e-6, `冠芽高 ${cb.size.y.toFixed(2)} 应 <= 果身高 ${bb.size.y.toFixed(2)} 的 45%`)
})

// ---------- Task 6: 人参与灵芝 ----------

test('人参：ginseng-root 叉状根、ginseng-compound-leaves 掌状复叶、成熟伞形红果', () => {
  const stageModel = build('renshen', stageCount(CROP_MAP.renshen) - 1)
  assert.ok(stageModel.getObjectByName('ginseng-root'), '大叶阶段应已有 ginseng-root')
  assert.ok(stageModel.getObjectByName('ginseng-compound-leaves'), '大叶阶段应已有掌状复叶')
  assert.equal(named(stageModel, 'ginseng-berries').length, 0, '未成熟不应出现红果簇')

  const model = build('renshen', 'mature')
  const root = model.getObjectByName('ginseng-root')
  const leaves = model.getObjectByName('ginseng-compound-leaves')
  assert.ok(root, '缺少 ginseng-root')
  assert.ok(leaves, '缺少 ginseng-compound-leaves')
  // 叉状根：根体由主根 + 叉根 + 侧根/须根多件组成，而非单一球块
  assert.ok(meshCount(root) >= 6, `人参根应由主根/叉根/侧根多件组成，实际 ${meshCount(root)} 件`)
  const rb = boundsOf(root)
  assert.ok(rb.size.y > rb.size.x, '人参根应为竖长分叉形而非圆球')
  // 掌状复叶：多枚复叶，每枚含多片小叶
  assert.ok(meshCount(leaves) >= 16, `掌状复叶应由多枚小叶组成，实际 ${meshCount(leaves)} 件`)
  const berries = named(model, 'ginseng-berries')
  assert.ok(berries.length >= 1, '成熟期应有 ginseng-berries 伞形红果簇')
  const reds = meshesWithColor(model, 0xd7263d)
  assert.ok(reds.length >= 5, `红果簇应不少于 5 粒，实际 ${reds.length}`)
  const berryY = Math.min(...reds.map((m) => worldPos(m).y))
  const leafCenterY = boundsOf(leaves).center.y
  assert.ok(berryY > leafCenterY, '红果簇应位于叶心上方')
})

test('灵芝：阶段 1-成熟均有 lingzhi-cap，大叶起扇面宽于菌柄，开花无花', () => {
  const total = stageCount(CROP_MAP.lingzhi)
  for (let st = 1; st < total; st++) {
    const model = build('lingzhi', st)
    assert.ok(model.getObjectByName('lingzhi-cap'), `灵芝阶段 ${st} 应存在 lingzhi-cap`)
  }
  const mature = build('lingzhi', 'mature')
  assert.ok(mature.getObjectByName('lingzhi-cap'), '灵芝成熟阶段应存在 lingzhi-cap')

  const bud = build('lingzhi', 0)
  assert.ok(meshesWithColor(bud, 0x8a5636).length >= 1, '发芽阶段菌蕾顶部应有小褐点')

  for (const st of [2, 3]) {
    const model = build('lingzhi', st)
    const cap = model.getObjectByName('lingzhi-cap')
    const stipe = model.getObjectByName('lingzhi-stipe')
    assert.ok(stipe, `阶段 ${st} 缺少 lingzhi-stipe`)
    const cw = boundsOf(cap).size.x
    const sw = boundsOf(stipe).size.x
    assert.ok(cw > sw * 1.8, `阶段 ${st} 菌盖宽 ${cw.toFixed(2)} 应明显大于菌柄宽 ${sw.toFixed(2)}`)
  }
  // 开花阶段：同心棕红环带，且不叠加花朵
  const flowering = build('lingzhi', 3)
  assert.ok(
    meshesWithColor(flowering, 0x8a4a3a).length >= 1 && meshesWithColor(flowering, 0xb0714f).length >= 1,
    '开花阶段菌盖应有同心棕红环带',
  )
  assert.ok(meshCount(flowering) < 15, `菌类开花阶段不应叠加花朵，实际 ${meshCount(flowering)} 件`)
  // 成熟：主扇面 + 带小柄的侧生小扇面
  const side = mature.getObjectByName('lingzhi-cap-side')
  assert.ok(side, '成熟灵芝应有侧生小扇面 lingzhi-cap-side')
  assert.ok(named(side, '').length === 0 || meshCount(side) >= 3, '侧生小扇面应含小柄与扇面多件')
  assert.equal(build('lingzhi', 3).getObjectByName('lingzhi-cap-side'), undefined, '开花阶段不应出现侧生小扇面')
})

// ---------- 第二轮修订：灵芝柱体、草莓果形、大白菜、藤蔓连续性 ----------

test('灵芝：各阶段菌盖抬升露出菌柄柱体，成熟整株不低于 0.9', () => {
  const total = stageCount(CROP_MAP.lingzhi)
  for (let st = 1; st < total; st++) {
    const model = build('lingzhi', st)
    const cap = model.getObjectByName('lingzhi-cap')
    const stipe = model.getObjectByName('lingzhi-stipe')
    assert.ok(cap && stipe, `阶段 ${st} 应有菌盖与菌柄`)
    const capBottom = boundsOf(cap).box.min.y
    assert.ok(capBottom > 0.28, `阶段 ${st} 菌盖底缘 ${capBottom.toFixed(2)} 应抬离地面露出菌柄`)
    const stipeTop = boundsOf(stipe).box.max.y
    assert.ok(capBottom <= stipeTop + 0.02, `阶段 ${st} 菌盖应由菌柄托举`)
  }
  const mature = build('lingzhi', 'mature')
  const mb = boundsOf(mature)
  assert.ok(mb.size.y >= 0.9, `灵芝成熟整株高 ${mb.size.y.toFixed(2)} 应 >= 0.9`)
  const stipe = mature.getObjectByName('lingzhi-stipe')
  assert.ok(boundsOf(stipe).size.y >= 0.7, `成熟菌柄高 ${boundsOf(stipe).size.y.toFixed(2)} 应 >= 0.7（柱子明显露出）`)
})

test('草莓：一体水滴形垂果（竖长轮廓），籽点分布整个果面', () => {
  const model = build('caomei', 'mature')
  const fruits = named(model, 'strawberry-fruit')
  assert.ok(fruits.length >= 4)
  for (const f of fruits) {
    const berry = f.getObjectByName('strawberry-berry')
    assert.ok(berry, '草莓果内应有 strawberry-berry 果体组（不含果梗）')
    const fb = boundsOf(berry)
    assert.ok(
      fb.size.y > Math.max(fb.size.x, fb.size.z) * 1.15,
      `草莓应为上宽下尖的竖长水滴形，实际 ${fb.size.x.toFixed(2)}/${fb.size.y.toFixed(2)}/${fb.size.z.toFixed(2)}`,
    )
    const reds = meshesWithColor(berry, 0xd9364b).length + meshesWithColor(berry, 0xe0646f).length
    assert.ok(reds >= 2, '草莓果体应由融合的球+锥组成连续轮廓')
    assert.ok(meshesWithColor(berry, 0xffe08a).length >= 8, '籽点应纵列分布整个果面（含锥部）')
  }
})

test('大白菜：napa-head 直立裹叶头高大于宽，外叶深绿抱拢', () => {
  const def = CROP_MAP.dabaicai
  const model = build('dabaicai', 'mature')
  const head = model.getObjectByName('napa-head')
  assert.ok(head, '缺少 napa-head 裹叶头')
  const hb = boundsOf(head)
  assert.ok(hb.size.y > Math.max(hb.size.x, hb.size.z), `裹叶头应高大于宽，实际 ${hb.size.x.toFixed(2)}/${hb.size.y.toFixed(2)}`)
  assert.ok(meshesWithColor(head, 0xd6e4b0).length >= 1, '裹叶头应为淡黄绿 0xd6e4b0')
  const outers = named(model, 'napa-outer-leaf')
  assert.ok(outers.length >= 3 && outers.length <= 4, `外叶应 3-4 片，实际 ${outers.length}`)
  for (const o of outers) {
    assert.ok(meshesWithColor(o, def.leaf).length >= 1, '外叶应使用深绿 def.leaf 与内叶形成色差')
  }
})

test('藤蔓类：小叶与大叶共用同一构件结构（主体 Mesh 数一致）', () => {
  const bodyMeshCount = (id, stageOrMature) => {
    const model = build(id, stageOrMature)
    return meshCount(model.children[0]) // createCropModel 始终先挂主体，再挂果实/花序
  }
  for (const id of ['wandou', 'putao', 'hulu', 'nangua', 'xigua']) {
    const total = stageCount(CROP_MAP[id])
    const young = bodyMeshCount(id, total - 2)   // 小叶/幼年期主体
    const grown = bodyMeshCount(id, total - 1)   // 大叶/开花期主体（不含花果）
    assert.equal(young, grown, `${id} 小叶与大叶应共用同一构件结构（${young} vs ${grown}）`)
  }
})

// ---------- 全局约束：确定性 ----------

test('核心结构不使用随机：同一作物同一阶段两次构建完全一致', () => {
  for (const id of TARGET_IDS) {
    const a = build(id, 'mature')
    const b = build(id, 'mature')
    assert.equal(meshCount(a), meshCount(b), `${id} 两次构建 Mesh 数应一致`)
    const sa = boundsOf(a).size
    const sb = boundsOf(b).size
    for (const k of ['x', 'y', 'z']) {
      assert.ok(Math.abs(sa[k] - sb[k]) < 1e-9, `${id} 两次构建包围盒 ${k} 应一致`)
    }
  }
})
