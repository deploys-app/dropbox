/**
 * format formats date to RFC3339 string
 * @param {import('dayjs').Dayjs} date
 * @returns {string}
 */
export function format (date) {
	return date.toISOString().replace(/\..+Z$/, 'Z')
}
