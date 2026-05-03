import { AccountForm } from './components/AccountForm';
import type { PluginFrontendModule } from '@airgate/theme/plugin';
import { ClaudeIcon } from './components/ClaudeIcon';

const plugin: PluginFrontendModule = {
  accountForm: AccountForm,
  platformIcon: ClaudeIcon,
};

export default plugin;
