import assert from 'node:assert/strict'
import test from 'node:test'

import { decodeBinaryBatch, encodeBinaryBatch } from './binaryWire.js'

test('binary batch round trip preserves decimal uint64 strings', () => {
  const encoded = encodeBinaryBatch([
    { cmd: 100, client_seq: 1, err: 0, payload: { uid: '18446744073709551615' } },
    { cmd: 204, client_seq: 2, err: 1002, payload: {} },
  ])
  assert.deepEqual(decodeBinaryBatch(encoded), [
    { cmd: 100, client_seq: 1, err: 0, payload: { uid: '18446744073709551615' } },
    { cmd: 204, client_seq: 2, err: 1002, payload: {} },
  ])
})

test('binary decoder rejects trailing bytes', () => {
  const encoded = encodeBinaryBatch([{ cmd: 100, client_seq: 1, err: 0, payload: {} }])
  const invalid = new Uint8Array(encoded.length + 1)
  invalid.set(encoded)
  assert.throws(() => decodeBinaryBatch(invalid), /trailing/)
})
