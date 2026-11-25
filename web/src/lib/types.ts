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

// Submissions and review (docs/api/submissions.md). Days are YYYY-MM-DD UTC.

export type SubmissionStatus = 'verifying' | 'ready' | 'failed' | 'withdrawn';
export type DecisionValue = 'approved' | 'adjusted' | 'rejected';

/** The program a submission is addressed to. A deleted program keeps its slug and name. */
export interface ProgramRef {
	slug: string;
	name: string;
}

/** What reviewers see of the person who submitted. */
export interface Submitter {
	username: string;
	display_name: string;
	/** null when Hackatime isn't linked. */
	hackatime_id: number | null;
}

/** The latest decision on a submission. */
export interface Decision {
	value: DecisionValue;
	/** Set for adjusted only. */
	approved_seconds: number | null;
	reason: string;
	/** The reviewer's username; null once their account is gone. */
	by: string | null;
	at: string;
}

/** Distinct submitted minutes by re-check result. */
export interface MinuteCounts {
	verified: number;
	mismatch: number;
	unbacked: number;
}

/** A submission in a list: the submitter's own, or a program's inbox (with `user`). */
export interface SubmissionSummary {
	id: string;
	project: string;
	from: string;
	to: string;
	/** Set in a program's inbox, never in the submitter's own list. */
	user?: Submitter;
	program: ProgramRef | null;
	status: SubmissionStatus;
	/** Why a failed submission failed; only the submitter sees it. */
	error?: string;
	counted_seconds: number;
	raw_seconds: number;
	minutes: MinuteCounts;
	decision: Decision | null;
	/** null once withdrawn. Never print it in full; copy it. */
	review_url: string | null;
	created_at: string;
}

export interface SubmissionDetail extends SubmissionSummary {
	packet: Packet | null;
}

/** A Hackatime trust level as programs see it (masked). */
export type TrustLevel = 'blue' | 'green' | 'red';

export interface PacketTrust {
	/** blue when Hackatime wasn't linked at submission. */
	at_submission: TrustLevel;
	now?: TrustLevel;
	checked_at?: string;
	hackatime_linked: boolean;
	hackatime_linked_now?: boolean;
	/** Absent when Hackatime wasn't linked at submission. */
	hackatime_account_changed?: boolean;
}

/** A cut or flag's minutes, merged into [start, end) RFC 3339 ranges. */
export type TimeRange = [string, string];

export interface PacketCut {
	code: string;
	rule: string;
	seconds: number;
	ranges: TimeRange[];
}

export interface PacketFlag {
	code: string;
	rule: string;
	minutes: number;
	ranges: TimeRange[];
}

export interface PacketCommit {
	at: string;
	/** The first 7 characters. */
	hash: string;
	files: number;
	added: number;
	removed: number;
	/** Only for a https://github.com/<owner>/<repo> project. */
	url?: string;
}

export interface PacketDay {
	day: string;
	/** One character per minute of the UTC day (the alphabet is in #lib/strip.ts). */
	strip: string;
	counted_seconds: number;
	raw_seconds: number;
	commits: PacketCommit[] | null;
}

export interface PacketAISession {
	harness: string;
	model: string;
	from: string;
	to: string;
	prompts: number;
	turns: number;
	counted_seconds: number;
	agent_alone_seconds: number;
}

export interface PacketCredit {
	label: string;
	credited: number;
	cuts: string[] | null;
}

export interface PacketMismatch {
	minute: string;
	device: PacketCredit;
	server: PacketCredit;
}

/** The review packet (submission.Packet in Go). Go may send null for an empty list. */
export interface Packet {
	rules_version: string;
	counted_seconds: number;
	trust: PacketTrust;
	raw_seconds: number;
	raw_ai_seconds: number;
	one_third_rule_seconds: number;
	minutes: MinuteCounts;
	devices: { name: string; os: string; gaps: string[] | null }[] | null;
	made_of: { editing: number; with_ai: number; other: number };
	cuts: PacketCut[] | null;
	flags: PacketFlag[] | null;
	days: PacketDay[] | null;
	ai_sessions: PacketAISession[] | null;
	mismatches: PacketMismatch[] | null;
	/** Holds the review link: show it masked, copy it in full. */
	justification?: string;
	raw_rows_deleted_after?: string;
}

/** GET /api/v1/review/{token}. While verifying (202) it has `submission` only. */
export interface Review {
	submission: {
		id: string;
		project: string;
		from: string;
		to: string;
		status: SubmissionStatus;
		user: Submitter;
		program: ProgramRef | null;
		created_at: string;
		/** The packet's version; a decision sends it back. */
		version: number;
	};
	packet?: Packet | null;
	decision?: Decision | null;
}

/** The body of POST /api/v1/review/{token}/decision. */
export type DecisionInput =
	| { value: 'approved'; version?: number }
	| { value: 'adjusted'; approved_seconds: number; reason: string; version?: number }
	| { value: 'rejected'; reason: string; version?: number };

/** Filters for a program's inbox. */
export interface InboxQuery {
	status?: SubmissionStatus;
	decision?: DecisionValue | 'none';
	limit?: number;
	offset?: number;
}

// Programs and OAuth (docs/api/programs.md).

export type AppRole = 'owner' | 'reviewer';

/** An app the user owns or reviews for. An app is also a program. */
export interface App {
	id: string;
	client_id: string;
	name: string;
	slug: string;
	redirect_uris: string[];
	homepage_url: string;
	public: boolean;
	confidential: boolean;
	role: AppRole;
	owner_username: string;
	created_at: string;
	updated_at: string;
	secret_rotated_at: string | null;
}

/** An app right after create or rotate: the secret is shown once and never stored. */
export interface AppWithSecret extends App {
	client_secret?: string;
}

export interface AppCreate {
	name: string;
	slug: string;
	redirect_uris: string[];
	homepage_url?: string;
	public: boolean;
	/** Defaults to true; false makes a public client (no secret, PKCE required). */
	confidential?: boolean;
}

export interface AppPatch {
	name?: string;
	redirect_uris?: string[];
	homepage_url?: string;
	public?: boolean;
}

export interface AppMember {
	username: string;
	role: AppRole;
	added_at: string;
	/** null for the owner, or once the adder's account is gone. */
	added_by: string | null;
}

/** A live public program, for the submit picker. */
export interface PublicProgram {
	slug: string;
	name: string;
	homepage_url: string;
}

/** A pending consent request, for the consent page. */
export interface ConsentRequest {
	app: { id: string; name: string; slug: string; homepage_url: string; owner_username: string };
	scopes: { name: string; description: string }[];
	redirect_uri: string;
	already_granted: boolean;
	expires_at: string;
}

/** An app the user has given access. */
export interface Grant {
	app_id: string;
	app_name: string;
	app_slug: string;
	homepage_url: string;
	owner_username: string;
	scopes: string[];
	created_at: string;
	updated_at: string;
}
