import dayjs from 'dayjs'
import { format as formatDate } from './date'
import { authorized } from './auth'

/**
 * @typedef Env
 * @property {import('@cloudflare/workers-types').R2Bucket} BUCKET
 * @property {import('@cloudflare/workers-types').AnalyticsEngineDataset} WAE
 */

const baseUrl = 'https://dropbox.deploys.app/'

export default {
	/**
	 * @param {import('@cloudflare/workers-types').Request} request
	 * @param {Env} env
	 * @param {import('@cloudflare/workers-types').ExecutionContext} ctx
	 * @returns {Promise<import('@cloudflare/workers-types').Response>}
	 **/
	async fetch (request, env, ctx) {
		if (request.method !== 'POST') {
			return new Response('Deploys.app Dropbox Service')
		}

		const auth = await authorized(request)
		if (!auth.authorized) {
			// TODO: reject request after alpha
			auth.authorized = true
			auth.project = {
				id: 'alpha',
				project: 'alpha'
			}
		}
		if (!auth.authorized) {
			return failResponse('api: unauthorized')
		}

		// Workers do not support matching route with query params,
		// so we use header to pass params instead
		let ttlDays = Number(request.headers.get('param-ttl'))
		if (!ttlDays || ttlDays < 1 || ttlDays > 7) {
			ttlDays = 1
		}
		const filename = request.headers.get('param-filename')

		const bodySize = Number(request.headers.get('content-length'))
		if (!request.body || bodySize === 0) {
			return failResponse('body empty')
		}

		const expiresAt = dayjs().add(ttlDays, 'day')

		const fn = ttlDays + generateFilename()
		try {
			await env.BUCKET.put(fn, request.body, {
				httpMetadata: {
					cacheControl: 'public, max-age=86400',
					contentDisposition: filename ? `attachment; filename="${escapeFilename(filename)}"` : undefined
				}
			})
		} catch (e) {
			console.error(e)
			return failResponse('failed to upload', 500)
		}

		// env.WAE undefined in test env
		env.WAE?.writeDataPoint({
			blobs: [
				auth.project.id,
				request.cf.colo,
				request.cf.country,
				ttlDays
			],
			doubles: [bodySize],
			indexes: [auth.project.id]
		})

		return new Response(JSON.stringify({
			ok: true,
			result: {
				downloadUrl: `${baseUrl}${fn}`,
				expiresAt: formatDate(expiresAt)
			}
		}), {
			headers: {
				'content-type': 'application/json'
			}
		})
	}
}

function generateFilename () {
	const fn = new Uint8Array(64)
	crypto.getRandomValues(fn)
	return toRawURLEncoding(btoa(String.fromCodePoint(...fn)))
}

function toRawURLEncoding (s) {
	return s.replace(/=*$/, '').replace(/\+/g, '-').replace(/\//g, '_')
}

/**
 * failResponse returns a JSON response with an error message
 * @param {string} msg
 * @param {number} status
 * @returns {Response}
 */
function failResponse (msg, status = 200) {
	return new Response(JSON.stringify({
		ok: false,
		error: {
			message: msg
		}
	}), {
		status,
		headers: {
			'content-type': 'application/json'
		}
	})
}

function escapeFilename (s) {
	s = s || ''
	return s.replace(/"/g, '')
}
