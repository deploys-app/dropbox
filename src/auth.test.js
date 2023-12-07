import { test, expect, describe, afterEach, vi } from 'vitest'
import { authorized } from './auth'

global.fetch = vi.fn()

describe('auth', () => {
	afterEach(() => {
		fetch.mockReset()
	})

	test('ok with project', async () => {
		const req = new Request('http://localhost/', {
			method: 'POST',
			headers: {
				authorization: 'bearer token',
				'param-project': 'test-project'
			}
		})

		fetch.mockResolvedValue({
			ok: true,
			json: async () => {
				return {
					ok: true,
					result: {
						authorized: true,
						project: {
							id: '1234567890',
							project: 'test-project',
							billingAccount: {
								active: true
							}
						}
					}
				}
			}
		})

		const r = await authorized(req)
		expect(fetch).toHaveBeenCalledWith('https://api.deploys.app/me.authorized', {
			method: 'POST',
			headers: {
				authorization: 'bearer token',
				'content-type': 'application/json'
			},
			body: JSON.stringify({
				project: 'test-project',
				permissions: ['dropbox.upload']
			})
		})
		expect(r.authorized).toBeTruthy()
		expect(r.project.id).toEqual('1234567890')
		expect(r.project.project).toEqual('test-project')
	})

	test('ok with projectId', async () => {
		const req = new Request('http://localhost/', {
			method: 'POST',
			headers: {
				authorization: 'bearer token',
				'param-project-id': '1234567890'
			}
		})

		fetch.mockResolvedValue({
			ok: true,
			json: async () => {
				return {
					ok: true,
					result: {
						authorized: true,
						project: {
							id: '1234567890',
							project: 'test-project',
							billingAccount: {
								active: true
							}
						}
					}
				}
			}
		})

		const r = await authorized(req)
		expect(fetch).toHaveBeenCalledWith('https://api.deploys.app/me.authorized', {
			method: 'POST',
			headers: {
				authorization: 'bearer token',
				'content-type': 'application/json'
			},
			body: JSON.stringify({
				projectId: '1234567890',
				permissions: ['dropbox.upload']
			})
		})
		expect(r.authorized).toBeTruthy()
		expect(r.project.id).toEqual('1234567890')
		expect(r.project.project).toEqual('test-project')
	})

	test('empty auth alpha', async () => {
		const req = new Request('http://localhost/', {
			method: 'POST'
		})
		const r = await authorized(req)
		expect(r.authorized).toBeTruthy()
		expect(r.project.id).toEqual('alpha')
		expect(r.project.project).toEqual('alpha')
	})

	// test('empty auth', async () => {
	// 	const req = new Request('http://localhost/', {
	// 		method: 'POST'
	// 	})
	// 	const r = await authorized(req)
	// 	expect(r.authorized).toBeFalsy()
	// 	expect(r.project).toBeUndefined()
	// })

	test('empty project and project id', async () => {
		const req = new Request('http://localhost/', {
			method: 'POST',
			headers: {
				authorization: 'bearer token'
			}
		})
		const r = await authorized(req)
		expect(r.authorized).toBeFalsy()
		expect(r.project).toBeUndefined()
	})

	test('fetch error', async () => {
		const req = new Request('http://localhost/', {
			method: 'POST',
			headers: {
				authorization: 'bearer token',
				'param-project-id': '1234567890'
			}
		})

		fetch.mockResolvedValue({
			ok: false
		})

		const r = await authorized(req)
		expect(r.authorized).toBeFalsy()
	})

	test('unauthorized from api', async () => {
		const req = new Request('http://localhost/', {
			method: 'POST',
			headers: {
				authorization: 'bearer token',
				'param-project-id': '1234567890'
			}
		})

		fetch.mockResolvedValue({
			ok: true,
			json: async () => {
				return {
					ok: true,
					result: {
						authorized: false,
						project: {
							id: '',
							project: ''
						}
					}
				}
			}
		})

		const r = await authorized(req)
		expect(r.authorized).toBeFalsy()
		expect(r.project).toBeUndefined()
	})

	test('api error', async () => {
		const req = new Request('http://localhost/', {
			method: 'POST',
			headers: {
				authorization: 'bearer token',
				'param-project-id': '1234567890'
			}
		})

		fetch.mockResolvedValue({
			ok: true,
			json: async () => {
				return {
					ok: false,
					error: {
						message: 'api error'
					}
				}
			}
		})

		const r = await authorized(req)
		expect(r.authorized).toBeFalsy()
		expect(r.project).toBeUndefined()
	})
})
