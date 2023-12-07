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

/**
 * Checks if the request is authorized based on the provided headers.
 *
 * @param {import('@cloudflare/workers-types').Request} request
 * @returns {Promise<AuthorizedResult>}
 */
export async function authorized (request) {
	const auth = request.headers.get('authorization')
	if (!auth) {
		return { authorized: false }
	}

	const project = request.headers.get('param-project')
	const projectId = request.headers.get('param-project-id')
	if (!project || !projectId) {
		return { authorized: false }
	}

	const resp = await fetch('https://api.deploys.app/me.authorized', {
		headers: {
			authorization: auth
		},
		body: JSON.stringify({
			project,
			projectId,
			permissions: [permission]
		})
	})
	if (!resp.ok) {
		return { authorized: false }
	}
	const res = await resp.json()
	if (!res.ok) {
		return { authorized: false }
	}
	return res.result
}
