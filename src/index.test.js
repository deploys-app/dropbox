import { test, expect, describe, beforeAll, afterAll, afterEach, vi } from 'vitest'
import { unstable_dev } from 'wrangler'
import dayjs from 'dayjs'
import { createMockAuthResponse } from './auth.test'

global.fetch = vi.fn()

expect.extend({
	dateTimeEqual (received, expected) {
		const receivedUnix = dayjs(received).unix()
		const expectedUnix = dayjs(expected).unix()
		return {
			pass: receivedUnix >= expectedUnix - 1 && receivedUnix <= expectedUnix + 1,
			message: () => `expected ${dayjs(received)} to equal ${dayjs(expected)}`
		}
	}
})

describe('dropbox', () => {
	let worker

	beforeAll(async () => {
		worker = await unstable_dev('src/index.js', {
			experimental: {
				disableExperimentalWarning: true
			}
		})
	})

	afterAll(async () => {
		await worker.stop()
	})

	afterEach(() => {
		fetch.mockReset()
	})

	test('get index', async () => {
		const resp = await worker.fetch('/')
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
	})

	test('get non index', async () => {
		const resp = await worker.fetch('/hello')
		expect(resp.ok).toBeFalsy()
		expect(resp.status).toEqual(404)
		expect(await resp.json()).toEqual({
			ok: false,
			error: {
				message: 'api: not found'
			}
		})
	})

	test('upload empty body', async () => {
		const resp = await worker.fetch('/', {
			method: 'POST'
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		expect(await resp.json()).toEqual({
			ok: false,
			error: {
				message: 'body empty'
			}
		})
	})

	test('upload file', async () => {
		const body = 'hello world'
		const resp = await worker.fetch('/', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.result.downloadUrl).toBeTruthy()
		expect(res.result.expiresAt).dateTimeEqual(dayjs().add(1, 'day'))
	})

	test('upload file with ttl query param', async () => {
		const body = 'hello world'
		const resp = await worker.fetch('/?ttl=4', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.result.downloadUrl).toBeTruthy()
		expect(res.result.expiresAt).dateTimeEqual(dayjs().add(4, 'day'))
	})

	test('upload file with ttl header', async () => {
		const body = 'hello world'
		const resp = await worker.fetch('/', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length,
				'param-ttl': 4
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.result.downloadUrl).toBeTruthy()
		expect(res.result.expiresAt).dateTimeEqual(dayjs().add(4, 'day'))
	})

	test('upload file with invalid ttl query param', async () => {
		const body = 'hello world'
		const resp = await worker.fetch('/?ttl=8', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.result.downloadUrl).toBeTruthy()
		expect(res.result.expiresAt).dateTimeEqual(dayjs().add(1, 'day'))
	})

	test('upload file with invalid ttl header', async () => {
		const body = 'hello world'
		const resp = await worker.fetch('/', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length,
				'param-ttl': 8
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.result.downloadUrl).toBeTruthy()
		expect(res.result.expiresAt).dateTimeEqual(dayjs().add(1, 'day'))
	})

	test('upload file with filename query param', async () => {
		const body = 'hello world'
		const resp = await worker.fetch('/?filename=hello.txt', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.result.downloadUrl).toBeTruthy()
	})

	test('upload file with filename header', async () => {
		const body = 'hello world'
		const resp = await worker.fetch('/', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length,
				'param-filename': 'hello.txt'
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.result.downloadUrl).toBeTruthy()
	})

	test('invalid auth query param', async () => {
		fetch.mockResolvedValue(createMockAuthResponse(true, {
			ok: true,
			result: {
				authorized: false,
				project: {
					id: '',
					project: ''
				}
			}
		}))

		const body = 'hello world'
		const resp = await worker.fetch('/?project=invalid&filename=hello.txt', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length,
				authorization: 'bearer invalid'
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.ok).toBeFalsy()
		expect(res.result).toBeUndefined()
		expect(res.error.message).toEqual('api: unauthorized')
	})

	test('invalid auth header', async () => {
		fetch.mockResolvedValue(createMockAuthResponse(true, {
			ok: true,
			result: {
				authorized: false,
				project: {
					id: '',
					project: ''
				}
			}
		}))

		const body = 'hello world'
		const resp = await worker.fetch('/', {
			method: 'POST',
			body,
			headers: {
				'content-length': body.length,
				'param-filename': 'hello.txt',
				authorization: 'bearer invalid',
				'param-project': 'invalid'
			}
		})
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
		const res = await resp.json()
		expect(res.ok).toBeFalsy()
		expect(res.result).toBeUndefined()
		expect(res.error.message).toEqual('api: unauthorized')
	})
})
