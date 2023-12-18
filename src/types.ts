declare global {
	export type Dayjs = import('dayjs').Dayjs

	export interface Env {
		BUCKET: R2Bucket
		WAE: AnalyticsEngineDataset
	}

	export type Project = {
		id: string
		project: string
	}

	export type AuthorizedResult =
		| { authorized: false; project?: never }
		| { authorized: true; project: Project }
}


export {}
