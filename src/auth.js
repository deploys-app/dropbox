const permission = 'dropbox.upload'

/**
 * @typedef Project
 * @property {string} id
 * @property {string} project
 */

/**
 * @typedef AuthorizedResult
 * @property {boolean} authorized
 * @property {Project?} project
 */

/** @type {AuthorizedResult} */
const unauthorizedResult = Object.freeze({
	authorized: false
})

/**
 * Checks if the request is authorized based on the provided headers.
 *
 * @param {import('@cloudflare/workers-types').Request} request
 * @returns {Promise<AuthorizedResult>}
 */
export async function authorized (request) {
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

	const project = request.headers.get('param-project') ?? ''
	const projectId = request.headers.get('param-project-id') ?? ''
	if (!project && !projectId) {
		return unauthorizedResult
	}

	const cacheKey = `deploys--dropbox|auth|${project}|${projectId}|${auth}`
	const resp = await fetch('https://api.deploys.app/me.authorized', {
		method: 'POST',
		headers: {
			authorization: auth,
			'content-type': 'application/json'
		},
		body: JSON.stringify({
			project: project || undefined,
			projectId: projectId || undefined,
			permissions: [permission]
		}),
		cf: {
			cacheTtlByStatus: {
				200: 30,
				'400-599': 0
			},
			cacheKey
		}
	})
	if (!resp.ok) {
		return unauthorizedResult
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
