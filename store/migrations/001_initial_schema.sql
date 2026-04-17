-- +goose Up
-- Initial schema: every table that used to be managed by GORM AutoMigrate.
-- Generated from a fresh AutoMigrate run via pg_dump, with psql meta-commands
-- and the pg_dump header SETs stripped. Notably: pg_dump emits
-- `SELECT set_config('search_path', '', ...)` which persists for the rest
-- of the transaction goose wraps us in and breaks goose's own
-- `INSERT INTO goose_db_version` afterward.

--
-- Name: accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.accounts (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    aws_account text NOT NULL,
    role_arn text NOT NULL,
    external_id text
);


--
-- Name: accounts_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.accounts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: accounts_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.accounts_id_seq OWNED BY public.accounts.id;


--
-- Name: agent_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agent_tokens (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    namespace_id bigint NOT NULL,
    token text NOT NULL,
    label text,
    revoked boolean DEFAULT false NOT NULL,
    metrics_enabled boolean DEFAULT false NOT NULL,
    metrics_interval text,
    metrics_collectors text
);


--
-- Name: agent_tokens_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.agent_tokens_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: agent_tokens_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.agent_tokens_id_seq OWNED BY public.agent_tokens.id;


--
-- Name: alert_incidents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.alert_incidents (
    id text NOT NULL,
    cron_watch_id bigint NOT NULL,
    condition text NOT NULL,
    opened_at timestamp with time zone NOT NULL,
    closed_at timestamp with time zone,
    last_seen_at timestamp with time zone NOT NULL,
    notified_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);


--
-- Name: cron_results; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    cron_watch_id bigint NOT NULL,
    ran_at timestamp with time zone NOT NULL,
    stdout text,
    stderr text,
    exit_code bigint,
    duration_ms bigint
);


--
-- Name: cron_results_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.cron_results_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: cron_results_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.cron_results_id_seq OWNED BY public.cron_results.id;


--
-- Name: cron_watches; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_watches (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    name text NOT NULL,
    schedule text NOT NULL,
    pipeline text NOT NULL,
    notify_dest text,
    enabled boolean DEFAULT true NOT NULL,
    last_run_at timestamp with time zone,
    last_status text
);


--
-- Name: cron_watches_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.cron_watches_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: cron_watches_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.cron_watches_id_seq OWNED BY public.cron_watches.id;


--
-- Name: customers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.customers (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text NOT NULL,
    display_name text
);


--
-- Name: customers_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.customers_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: customers_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.customers_id_seq OWNED BY public.customers.id;


--
-- Name: device_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.device_codes (
    id bigint NOT NULL,
    device_code text NOT NULL,
    user_code text NOT NULL,
    user_id text,
    expires_at timestamp with time zone,
    created_at timestamp with time zone
);


--
-- Name: device_codes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.device_codes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: device_codes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.device_codes_id_seq OWNED BY public.device_codes.id;


--
-- Name: log_groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.log_groups (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    account_id bigint NOT NULL,
    app text NOT NULL,
    log_group_name text NOT NULL,
    region text DEFAULT 'us-east-2'::text NOT NULL,
    schema_yaml text NOT NULL,
    poll_interval text DEFAULT '30s'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    last_poll_at text,
    last_token text
);


--
-- Name: log_groups_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.log_groups_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: log_groups_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.log_groups_id_seq OWNED BY public.log_groups.id;


--
-- Name: magic_links; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.magic_links (
    id bigint NOT NULL,
    email text NOT NULL,
    token text NOT NULL,
    expires_at timestamp with time zone,
    used boolean DEFAULT false NOT NULL
);


--
-- Name: magic_links_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.magic_links_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: magic_links_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.magic_links_id_seq OWNED BY public.magic_links.id;


--
-- Name: namespace_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.namespace_permissions (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    user_id text NOT NULL,
    namespace_id bigint NOT NULL,
    can_read boolean DEFAULT true NOT NULL,
    can_create_tokens boolean DEFAULT false NOT NULL,
    can_manage_watches boolean DEFAULT false NOT NULL,
    can_manage_functions boolean DEFAULT false NOT NULL,
    can_manage_integrations boolean DEFAULT false NOT NULL
);


--
-- Name: namespace_permissions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.namespace_permissions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: namespace_permissions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.namespace_permissions_id_seq OWNED BY public.namespace_permissions.id;


--
-- Name: namespaces; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.namespaces (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    name text NOT NULL
);


--
-- Name: namespaces_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.namespaces_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: namespaces_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.namespaces_id_seq OWNED BY public.namespaces.id;


--
-- Name: notify_configs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.notify_configs (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    slack_webhook text,
    slack_bot_token text,
    slack_team_name text,
    pager_duty_keys text,
    routes text,
    configured_at timestamp with time zone
);


--
-- Name: notify_configs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.notify_configs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: notify_configs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.notify_configs_id_seq OWNED BY public.notify_configs.id;


--
-- Name: pending_sources; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pending_sources (
    customer_id bigint NOT NULL,
    namespace text NOT NULL,
    app text NOT NULL,
    instance text,
    host_group text,
    sample jsonb NOT NULL,
    detection jsonb NOT NULL,
    created_at timestamp with time zone
);


--
-- Name: saved_queries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.saved_queries (
    id text NOT NULL,
    user_id text NOT NULL,
    name text NOT NULL,
    query text NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);


--
-- Name: shard_records; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_records (
    id text NOT NULL,
    apps text NOT NULL,
    window_start text NOT NULL,
    window_end text NOT NULL,
    state text NOT NULL,
    addr text,
    instance_id text,
    parquet_path text,
    tier text,
    row_count bigint DEFAULT 0,
    created_at text NOT NULL
);


--
-- Name: shell_functions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shell_functions (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    name text NOT NULL,
    source text NOT NULL,
    language text DEFAULT 'bash'::text NOT NULL
);


--
-- Name: shell_functions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.shell_functions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: shell_functions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.shell_functions_id_seq OWNED BY public.shell_functions.id;


--
-- Name: subscriptions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.subscriptions (
    id text NOT NULL,
    customer_id text NOT NULL,
    tier text NOT NULL,
    status text NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone,
    stripe_subscription_id text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);


--
-- Name: team_members; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_members (
    id text NOT NULL,
    customer_name text NOT NULL,
    email text NOT NULL,
    user_id text,
    role text DEFAULT 'member'::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    invited_by text,
    invited_at timestamp with time zone,
    accepted_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id text NOT NULL,
    email text NOT NULL,
    customer_id bigint NOT NULL,
    name text,
    stripe_customer_id text,
    can_create_namespace boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);


--
-- Name: accounts id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts ALTER COLUMN id SET DEFAULT nextval('public.accounts_id_seq'::regclass);


--
-- Name: agent_tokens id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens ALTER COLUMN id SET DEFAULT nextval('public.agent_tokens_id_seq'::regclass);


--
-- Name: cron_results id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ALTER COLUMN id SET DEFAULT nextval('public.cron_results_id_seq'::regclass);


--
-- Name: cron_watches id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watches ALTER COLUMN id SET DEFAULT nextval('public.cron_watches_id_seq'::regclass);


--
-- Name: customers id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.customers ALTER COLUMN id SET DEFAULT nextval('public.customers_id_seq'::regclass);


--
-- Name: device_codes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_codes ALTER COLUMN id SET DEFAULT nextval('public.device_codes_id_seq'::regclass);


--
-- Name: log_groups id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups ALTER COLUMN id SET DEFAULT nextval('public.log_groups_id_seq'::regclass);


--
-- Name: magic_links id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.magic_links ALTER COLUMN id SET DEFAULT nextval('public.magic_links_id_seq'::regclass);


--
-- Name: namespace_permissions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions ALTER COLUMN id SET DEFAULT nextval('public.namespace_permissions_id_seq'::regclass);


--
-- Name: namespaces id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespaces ALTER COLUMN id SET DEFAULT nextval('public.namespaces_id_seq'::regclass);


--
-- Name: notify_configs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_configs ALTER COLUMN id SET DEFAULT nextval('public.notify_configs_id_seq'::regclass);


--
-- Name: shell_functions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shell_functions ALTER COLUMN id SET DEFAULT nextval('public.shell_functions_id_seq'::regclass);


--
-- Name: accounts accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_pkey PRIMARY KEY (id);


--
-- Name: agent_tokens agent_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT agent_tokens_pkey PRIMARY KEY (id);


--
-- Name: alert_incidents alert_incidents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.alert_incidents
    ADD CONSTRAINT alert_incidents_pkey PRIMARY KEY (id);


--
-- Name: cron_results cron_results_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results
    ADD CONSTRAINT cron_results_pkey PRIMARY KEY (id);


--
-- Name: cron_watches cron_watches_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watches
    ADD CONSTRAINT cron_watches_pkey PRIMARY KEY (id);


--
-- Name: customers customers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.customers
    ADD CONSTRAINT customers_pkey PRIMARY KEY (id);


--
-- Name: device_codes device_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_codes
    ADD CONSTRAINT device_codes_pkey PRIMARY KEY (id);


--
-- Name: log_groups log_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT log_groups_pkey PRIMARY KEY (id);


--
-- Name: magic_links magic_links_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.magic_links
    ADD CONSTRAINT magic_links_pkey PRIMARY KEY (id);


--
-- Name: namespace_permissions namespace_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions
    ADD CONSTRAINT namespace_permissions_pkey PRIMARY KEY (id);


--
-- Name: namespaces namespaces_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespaces
    ADD CONSTRAINT namespaces_pkey PRIMARY KEY (id);


--
-- Name: notify_configs notify_configs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_configs
    ADD CONSTRAINT notify_configs_pkey PRIMARY KEY (id);


--
-- Name: saved_queries saved_queries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.saved_queries
    ADD CONSTRAINT saved_queries_pkey PRIMARY KEY (id);


--
-- Name: shard_records shard_records_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_records
    ADD CONSTRAINT shard_records_pkey PRIMARY KEY (id);


--
-- Name: shell_functions shell_functions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shell_functions
    ADD CONSTRAINT shell_functions_pkey PRIMARY KEY (id);


--
-- Name: subscriptions subscriptions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);


--
-- Name: team_members team_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_members
    ADD CONSTRAINT team_members_pkey PRIMARY KEY (id);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: idx_account_app; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_account_app ON public.log_groups USING btree (account_id, app);


--
-- Name: idx_accounts_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_deleted_at ON public.accounts USING btree (deleted_at);


--
-- Name: idx_agent_tokens_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_agent_tokens_deleted_at ON public.agent_tokens USING btree (deleted_at);


--
-- Name: idx_agent_tokens_token; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_agent_tokens_token ON public.agent_tokens USING btree (token);


--
-- Name: idx_cron_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_cron_customer_name ON public.cron_watches USING btree (customer_id, name);


--
-- Name: idx_cron_results_cron_watch_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_results_cron_watch_id ON public.cron_results USING btree (cron_watch_id);


--
-- Name: idx_cron_results_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_results_deleted_at ON public.cron_results USING btree (deleted_at);


--
-- Name: idx_cron_watches_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_watches_deleted_at ON public.cron_watches USING btree (deleted_at);


--
-- Name: idx_customers_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_customers_deleted_at ON public.customers USING btree (deleted_at);


--
-- Name: idx_customers_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_customers_name ON public.customers USING btree (name);


--
-- Name: idx_device_codes_device_code; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_device_codes_device_code ON public.device_codes USING btree (device_code);


--
-- Name: idx_device_codes_user_code; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_device_codes_user_code ON public.device_codes USING btree (user_code);


--
-- Name: idx_func_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_func_customer_name ON public.shell_functions USING btree (customer_id, name);


--
-- Name: idx_incident_watch_condition; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_incident_watch_condition ON public.alert_incidents USING btree (cron_watch_id, condition);


--
-- Name: idx_log_groups_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_log_groups_deleted_at ON public.log_groups USING btree (deleted_at);


--
-- Name: idx_magic_links_token; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_magic_links_token ON public.magic_links USING btree (token);


--
-- Name: idx_namespace_permissions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_namespace_permissions_deleted_at ON public.namespace_permissions USING btree (deleted_at);


--
-- Name: idx_namespaces_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_namespaces_deleted_at ON public.namespaces USING btree (deleted_at);


--
-- Name: idx_notify_configs_customer_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_notify_configs_customer_id ON public.notify_configs USING btree (customer_id);


--
-- Name: idx_notify_configs_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notify_configs_deleted_at ON public.notify_configs USING btree (deleted_at);


--
-- Name: idx_ns_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ns_customer_name ON public.namespaces USING btree (customer_id, name);


--
-- Name: idx_nsperm_user_ns; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_nsperm_user_ns ON public.namespace_permissions USING btree (user_id, namespace_id);


--
-- Name: idx_pending_customer_ns_app; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_pending_customer_ns_app ON public.pending_sources USING btree (customer_id, namespace, app);


--
-- Name: idx_saved_queries_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_saved_queries_user_id ON public.saved_queries USING btree (user_id);


--
-- Name: idx_shell_functions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shell_functions_deleted_at ON public.shell_functions USING btree (deleted_at);


--
-- Name: idx_subscriptions_customer_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_subscriptions_customer_id ON public.subscriptions USING btree (customer_id);


--
-- Name: idx_team_members_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_team_members_customer_name ON public.team_members USING btree (customer_name);


--
-- Name: idx_team_members_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_team_members_user_id ON public.team_members USING btree (user_id);


--
-- Name: idx_user_email_customer; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_user_email_customer ON public.users USING btree (email, customer_id);


--
-- Name: log_groups fk_accounts_log_groups; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT fk_accounts_log_groups FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: agent_tokens fk_agent_tokens_namespace; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT fk_agent_tokens_namespace FOREIGN KEY (namespace_id) REFERENCES public.namespaces(id) ON DELETE CASCADE;


--
-- Name: accounts fk_customers_accounts; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT fk_customers_accounts FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: agent_tokens fk_customers_agent_tokens; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT fk_customers_agent_tokens FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: namespaces fk_customers_namespaces; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespaces
    ADD CONSTRAINT fk_customers_namespaces FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: pending_sources fk_pending_sources_customer; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_sources
    ADD CONSTRAINT fk_pending_sources_customer FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: users fk_users_customer; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT fk_users_customer FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
--


