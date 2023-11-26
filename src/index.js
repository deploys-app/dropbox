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

		const url = new URL(request.url)
		if (url.pathname !== '/') {
			return new Response('error: not found', { status: 404 })
		}
		const bodySize = Number(request.headers.get('content-length'))
		if (!request.body || bodySize === 0) {
			return new Response('error: body empty', { status: 400 })
		}

		// TODO: check auth

		const fn = generateFilename()
		try {
			await env.BUCKET.put(fn, request.body, {
				httpMetadata: {
					cacheControl: 'public, max-age=86400'
				}
			})
		} catch (e) {
			console.error(e)
			return new Response('error: failed to upload', { status: 500 })
		}

		// env.WAE undefined in test env
		env.WAE?.writeDataPoint({
			blobs: [
				'alpha', // TODO: project id
				request.cf.colo,
				request.cf.country
			],
			doubles: [bodySize],
			indexes: ['alpha']
		})

		return new Response(JSON.stringify({
			downloadUrl: `${baseUrl}${fn}`
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
