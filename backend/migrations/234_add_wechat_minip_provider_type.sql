-- 微信小程序一键登录(wechat_minip):把 provider type 加进各 CHECK 约束。
--
-- 与 136_add_dingtalk_provider_type.sql 同构。Ent schema 早已支持该枚举
-- (ent/schema/user.go 的 signup_source 校验、ent/schema/auth_identity.go 的 provider type),
-- 但这两处 CHECK 约束由迁移管 —— 漏了它会让微信登录在写入时报:
--   violates check constraint "users_signup_source_check"
--   violates check constraint "auth_identities_provider_type_check"
-- 表现为登录能成功但 signup_source 停留在 email、微信身份记录写不进去。
--
-- 四个约束一并补齐:当前小程序流程只写前两个(auth_identities 与 users),
-- 但后两个(pending 会话、身份-渠道绑定)将来若用到 wechat_minip 不该再踩一次。

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_signup_source_check;

ALTER TABLE users
    ADD CONSTRAINT users_signup_source_check
    CHECK (signup_source IN ('email', 'linuxdo', 'wechat', 'oidc', 'github', 'google', 'dingtalk', 'wechat_minip'));

ALTER TABLE auth_identities
    DROP CONSTRAINT IF EXISTS auth_identities_provider_type_check;

ALTER TABLE auth_identities
    ADD CONSTRAINT auth_identities_provider_type_check
    CHECK (provider_type IN ('email', 'linuxdo', 'wechat', 'oidc', 'github', 'google', 'dingtalk', 'wechat_minip'));

ALTER TABLE auth_identity_channels
    DROP CONSTRAINT IF EXISTS auth_identity_channels_provider_type_check;

ALTER TABLE auth_identity_channels
    ADD CONSTRAINT auth_identity_channels_provider_type_check
    CHECK (provider_type IN ('email', 'linuxdo', 'wechat', 'oidc', 'github', 'google', 'dingtalk', 'wechat_minip'));

ALTER TABLE pending_auth_sessions
    DROP CONSTRAINT IF EXISTS pending_auth_sessions_provider_type_check;

ALTER TABLE pending_auth_sessions
    ADD CONSTRAINT pending_auth_sessions_provider_type_check
    CHECK (provider_type IN ('email', 'linuxdo', 'wechat', 'oidc', 'github', 'google', 'dingtalk', 'wechat_minip'));
