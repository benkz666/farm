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

test('all public request commands round trip through typed protobuf payloads', () => {
  const commands = [
    [100, { token: 'session-token', client_config_ver: 1, resume_farm_uid: 9, resume_farm_seq: 7 }],
    [102, { client_time: 123 }],
    [200, { owner_uid: 9 }],
    [202, {}],
    [204, { owner_uid: 9, from_seq: 7 }],
    ...[206, 208, 210, 212, 214, 216, 218, 220].map((cmd) => [cmd, { owner_uid: 9, plot_index: 3, arg: 1 }]),
    [222, { owner_uid: 9, plot_index: 3, crop_id: 1 }],
    [302, { item_id: 1, quantity: 2 }],
    [304, { item_id: 1, quantity: 2 }],
    [400, {}],
    [402, {}],
    [404, { token: 'invite-token' }],
    [406, { peer_uid: 9 }],
    [408, { peer_uid: 9 }],
    [410, { username: 'alice' }],
    [412, { peer_uid: 9 }],
    [414, {}],
    [416, { from_uid: 9 }],
    [418, { from_uid: 9 }],
    [500, {}],
    [502, { dog_type: 1 }],
    [504, { grams: 10 }],
    [600, {}],
    [602, { task_id: 2 }],
    [604, {}],
    [606, { all: true }],
    [608, { mail_id: '9007199254740993' }],
    [610, { all: true }],
    [612, {}],
    [614, {}],
    [616, { time_profile: 'demo' }],
  ]
  const envelopes = commands.map(([cmd, payload], index) => ({ cmd, client_seq: index + 1, err: 0, payload }))
  assert.deepEqual(decodeBinaryBatch(encodeBinaryBatch(envelopes)), envelopes)
})
