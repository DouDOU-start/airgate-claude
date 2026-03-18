import { AccountForm } from './components/AccountForm';
import type { AccountFormProps } from './components/AccountForm';

/** 插件前端模块导出 */
export interface PluginFrontendModule {
  accountForm?: React.ComponentType<AccountFormProps>;
}

const plugin: PluginFrontendModule = {
  accountForm: AccountForm,
};

export default plugin;
