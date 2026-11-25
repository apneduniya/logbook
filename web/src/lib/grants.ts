// Words for the apps a user has given access (Settings → Apps with access,
// docs/api/programs.md "User grants"). Pure.

/**
 * What each scope lets an app read, in a few words. The consent page shows
 * the server's own sentence for each; this is its short form for a list row.
 */
const SCOPE_WORDS: Record<string, string> = {
	profile: 'profile',
	read: 'activity'
};

/** "profile and activity"; an unknown scope reads as its name. */
export function scopeWords(scopes: string[]): string {
	const words = [...new Set(scopes.map((s) => SCOPE_WORDS[s] ?? s))];
	if (words.length <= 1) return words[0] ?? 'nothing';
	return `${words.slice(0, -1).join(', ')} and ${words[words.length - 1]}`;
}

/** "You've given 2 apps access." */
export function grantsSentence(n: number): string {
	if (n === 0) return 'No app can read your Logbook data.';
	return `You’ve given ${n === 1 ? 'one app' : `${n} apps`} access.`;
}
