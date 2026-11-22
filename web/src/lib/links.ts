// Outside links the UI points at.

export const REPO_URL = 'https://github.com/apneduniya/logbook';
export const RULES_URL = `${REPO_URL}/blob/main/docs/STRUGGLE-RULES.md`;

const INSTALL_BASE = 'https://raw.githubusercontent.com/apneduniya/logbook/main/scripts';

/**
 * The one-line installers (docs/agent/install.md). The key and the API origin
 * go in env vars (never --key, which shows in the process list), so the agent
 * reports to the same API this page talks to.
 */
export function installCommand(os: 'unix' | 'windows', key: string, server: string): string {
	return os === 'windows'
		? `$env:LOGBOOK_API_KEY="${key}"; $env:LOGBOOK_SERVER="${server}"; irm ${INSTALL_BASE}/install.ps1 | iex`
		: `curl -fsSL ${INSTALL_BASE}/install.sh | LOGBOOK_API_KEY=${key} LOGBOOK_SERVER=${server} sh`;
}
