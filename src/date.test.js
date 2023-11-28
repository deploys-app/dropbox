import { describe, expect, test } from 'vitest'
import dayjs from 'dayjs'
import { format } from './date'

describe('date', () => {
	test('format', () => {
		const d = dayjs('2020-01-01T00:00:00Z')
		expect(format(d)).toEqual('2020-01-01T00:00:00Z')
	})
})
