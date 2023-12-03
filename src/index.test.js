import { test, expect, describe, beforeAll, afterAll } from 'vitest'
import { unstable_dev } from 'wrangler'
import dayjs from 'dayjs'

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

	test('get index', async () => {
		const resp = await worker.fetch('/')
		expect(resp.ok).toBeTruthy()
		expect(resp.status).toEqual(200)
	})

	test('upload empty body', async () => {
		const resp = await worker.fetch('/', {
			method: 'POST'
		})
		expect(resp.ok).toBeFalsy()
		expect(resp.status).toEqual(400)
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

	test('upload file with ttl', async () => {
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

	test('upload file with invalid ttl', async () => {
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

	test('upload file with filename', async () => {
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
})
