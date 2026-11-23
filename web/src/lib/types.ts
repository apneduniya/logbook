// Wire types of the Logbook JSON API (docs/api/*.md). Times are RFC 3339 UTC
// strings; dates are YYYY-MM-DD in the user's time zone.

export interface User {
	id: string;
	username: string;
	display_name: string;
	email: string;
	avatar_url: string;
	timezone: string;
	profile_public: boolean;
	role: string;
	created_at: string;
}

export interface Session {
	user: User;
	csrf_token: string;
	expires_at: string;
}

export interface Named {
	name: string;
	seconds: number;
}

export interface Labels {
	you: number;
	agent: number;
	idle: number;
}

export interface Day {
	date: string;
	seconds: number;
	credited_seconds: number;
}

export interface Summary {
	start: string;
	end: string;
	timezone: string;
	total_seconds: number;
	labels: Labels;
	credited_seconds: number | null;
	credited_labels: Labels | null;
	multi_device_minutes: number;
	projects: { name: string; seconds: number; credited_seconds: number | null }[];
	languages: Named[];
	editors: Named[];
	categories: Named[];
	days: Day[];
}

export interface Today extends Summary {
	streak: { current: number; longest: number };
}

export interface Project {
	name: string;
	total_seconds: number;
	credited_seconds: number | null;
	last_activity: string | null;
	languages: Named[];
	repo_url: string;
	hidden: boolean;
	archived: boolean;
}

export interface ProjectPatch {
	repo_url?: string;
	hidden?: boolean;
	archived?: boolean;
}

export interface Profile {
	username: string;
	display_name: string;
	avatar_url: string;
	timezone: string;
	profile_public: boolean;
	member_since: string;
}

export interface ProfilePatch {
	display_name?: string;
	timezone?: string;
	profile_public?: boolean;
}

export interface PublicProfile {
	username: string;
	display_name: string;
	avatar_url: string;
	member_since: string;
	totals: {
		all_time_seconds: number;
		last_7_days_seconds: number;
		credited_all_time_seconds: number | null;
		credited_last_7_days_seconds: number | null;
	};
	projects: {
		name: string;
		repo_url: string;
		total_seconds: number;
		last_7_days_seconds: number;
		credited_seconds: number | null;
		credited_last_7_days_seconds: number | null;
	}[];
	languages: Named[];
}

export interface ApiKey {
	id: string;
	name: string;
	prefix: string;
	created_at: string;
	last_used_at: string | null;
}

export interface NewApiKey extends ApiKey {
	key: string;
}

export interface Device {
	id: string;
	name: string;
	os: string;
	public_key: string;
	key_fingerprint: string;
	created_at: string;
	last_seen_at: string | null;
	revoked_at: string | null;
}

export interface HackatimeImport {
	status: 'pending' | 'running' | 'done' | 'failed';
	from_date: string;
	to_date: string;
	next_date: string;
	recent_from: string;
	recent_done: boolean;
	days_total: number;
	days_done: number;
	heartbeats_imported: number;
	last_error: string;
	updated_at: string;
}

export type HackatimeLink =
	| { linked: false }
	| {
			linked: true;
			hackatime_user_id: number;
			hackatime_username?: string;
			trust_level: 'blue' | 'green' | 'red' | 'unknown';
			trust_checked_at: string;
			linked_at: string;
			import?: HackatimeImport;
	  };

/** The dashboard's range switch. */
export type Range = '7d' | '30d' | 'year';
