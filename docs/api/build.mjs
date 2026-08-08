import { createRequire } from 'node:module'
import { execFileSync } from 'node:child_process'
import { cpSync, existsSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '../..')
const trackedOutput = join(root, 'server/gateway/apidocs/static')
const checking = process.argv.includes('--check')
const tempRoot = checking ? mkdtempSync(join(tmpdir(), 'farm-api-docs-')) : null
const output = checking ? join(tempRoot, 'static') : trackedOutput
const require = createRequire(import.meta.url)
const AsyncAPIGenerator = require('@asyncapi/generator')

if (!checking) rmSync(output, { recursive: true, force: true })
mkdirSync(output, { recursive: true })

execFileSync('python3', [
  join(root, 'tools/api-docs/main.py'),
  '--root', root,
  '--output', output,
], { stdio: 'inherit' })

cpSync(join(here, 'openapi.yaml'), join(output, 'openapi.yaml'))

const swaggerDist = require('swagger-ui-dist').absolutePath()
const assetsDir = join(output, 'assets')
mkdirSync(assetsDir, { recursive: true })
for (const name of ['swagger-ui.css', 'swagger-ui-bundle.js', 'swagger-ui-standalone-preset.js']) {
  cpSync(join(swaggerDist, name), join(assetsDir, name))
}
const httpDir = join(output, 'http')
mkdirSync(httpDir, { recursive: true })
writeFileSync(join(httpDir, 'index.html'), `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>经典农场 HTTP API</title><link rel="stylesheet" href="../assets/swagger-ui.css"></head>
<body><div id="swagger-ui"></div><script src="../assets/swagger-ui-bundle.js"></script>
<script src="../assets/swagger-ui-standalone-preset.js"></script><script>
window.onload=()=>SwaggerUIBundle({url:'../openapi.yaml',dom_id:'#swagger-ui',deepLinking:true,
validatorUrl:null,presets:[SwaggerUIBundle.presets.apis,SwaggerUIStandalonePreset],layout:'StandaloneLayout'});
</script></body></html>\n`)

// Generate an offline, single-file AsyncAPI reference. The Python generator has
// already emitted a small fallback page, so failures still leave a useful contract.
const asyncapi = join(here, 'node_modules/.bin/asyncapi')
if (existsSync(asyncapi)) {
  const wsDir = join(output, 'ws')
  rmSync(wsDir, { recursive: true, force: true })
  const generator = new AsyncAPIGenerator(join(here, 'node_modules/@asyncapi/html-template'), wsDir, {
    forceWrite: true,
    templateParams: { singleFile: 'true' },
  })
  await generator.generateFromFile(join(output, 'asyncapi.yaml'))
}

function compareTrees(expected, actual, prefix = '') {
  const expectedEntries = readdirSync(expected, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))
  const actualEntries = readdirSync(actual, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))
  const left = expectedEntries.map((entry) => `${entry.isDirectory() ? 'd' : 'f'}:${entry.name}`)
  const right = actualEntries.map((entry) => `${entry.isDirectory() ? 'd' : 'f'}:${entry.name}`)
  if (JSON.stringify(left) !== JSON.stringify(right)) {
    throw new Error(`api docs drift at ${prefix || '.'}: ${left.join(',')} != ${right.join(',')}`)
  }
  for (const entry of expectedEntries) {
    const relative = join(prefix, entry.name)
    if (entry.isDirectory()) compareTrees(join(expected, entry.name), join(actual, entry.name), relative)
    else if (!readFileSync(join(expected, entry.name)).equals(readFileSync(join(actual, entry.name)))) {
      throw new Error(`api docs drift: ${relative}`)
    }
  }
}

if (checking) {
  if (!existsSync(trackedOutput)) throw new Error('generated API docs are missing; run make api-docs')
  compareTrees(output, trackedOutput)
  rmSync(tempRoot, { recursive: true, force: true })
  process.stdout.write('api-docs-check: OK\n')
}
