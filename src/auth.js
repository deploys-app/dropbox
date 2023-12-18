const permission = 'dropbox.upload'

/** @type {AuthorizedResult} */
const unauthorizedResult = Object.freeze({
	authorized: false
})

const authEndpoint = 'https://api.deploys.app/me.authorized'

/**
 * Checks if the request is authorized based on the provided headers.
 *
 * @param {Request} request
 * @param {ExecutionContext} ctx
 * @returns {Promise<AuthorizedResult>}
 */
export async function authorized (request, ctx) {
	const auth = request.headers.get('authorization')
	if (!auth) {
		// TODO: remove after alpha
		return {
			authorized: true,
			project: {
				id: 'alpha',
				project: 'alpha'
			}
		}
	}
	const url = new URL(request.url)

	const project = url.searchParams.get('project') ?? request.headers.get('param-project') ?? ''
	const projectId = url.searchParams.get('projectId') ?? request.headers.get('param-project-id') ?? ''
	if (!project && !projectId) {
		return unauthorizedResult
	}

	const cache = caches.default
	const cacheKey = `deploys--dropbox|auth|${project}|${projectId}|${auth}`
	const cacheReq = new Request(authEndpoint, {
		cf: {
			cacheTtl: 30,
			cacheKey,
			cacheTags: ['deploys--dropbox|auth']
		}
	})
	let resp = await cache.match(cacheReq)
	if (!resp) {
		resp = await fetch(authEndpoint, {
			method: 'POST',
			headers: {
				authorization: auth,
				'content-type': 'application/json'
			},
			body: JSON.stringify({
				project: project || undefined,
				projectId: projectId || undefined,
				permissions: [permission]
			})
		})
		if (!resp.ok) {
			return unauthorizedResult
		}

		// cache
		const cacheResp = new Response(resp.clone().body, resp)
		cacheResp.headers.set('cache-control', 'public, max-age=30')
		ctx.waitUntil(cache.put(cacheReq, cacheResp))
	}

	const res = await resp.json()
	if (!res.ok) {
		return unauthorizedResult
	}
	if (!res.result.authorized) {
		return unauthorizedResult
	}
	if (!res.result.project.billingAccount.active) {
		return unauthorizedResult
	}

	return {
		authorized: true,
		project: {
			id: res.result.project.id,
			project: res.result.project.project
		}
	}
}
