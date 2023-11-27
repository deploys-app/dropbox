import { test, expect, describe, beforeAll, afterAll } from 'vitest'
import { unstable_dev } from 'wrangler'

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

	test('post 404', async () => {
		const resp = await worker.fetch('/invalid', {
			method: 'POST'
		})
		expect(resp.ok).toBeFalsy()
		expect(resp.status).toEqual(404)
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
	})
})
