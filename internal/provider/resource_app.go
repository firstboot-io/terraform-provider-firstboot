package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/go-sdk/fbapi"
)

// An app is two lifecycles wearing one name, and this file keeps them apart.
//
// The APP is a long-lived thing with a plan, a replica count and a set of
// environment variables. A BUILD is a job that turns a repository into an image;
// it has its own states, its own failures and its own duration. Watching the app
// to find out whether a deploy worked shows nothing, because an app that is
// already running stays `running` throughout a build that fails.
//
// So a create that carries a repository does two waits: the app, then its first
// build. And an update that changes the source waits for the build that change
// queued -- found by comparing the build list before and after, because the
// source endpoint answers with the app rather than with the build it started.
var (
	_ resource.Resource                = (*appResource)(nil)
	_ resource.ResourceWithConfigure   = (*appResource)(nil)
	_ resource.ResourceWithImportState = (*appResource)(nil)
)

func NewAppResource() resource.Resource { return &appResource{} }

type appResource struct{ resourceConfigure }

type appEnvModel struct {
	Key    types.String `tfsdk:"key"`
	Value  types.String `tfsdk:"value"`
	Phase  types.String `tfsdk:"phase"`
	Secret types.Bool   `tfsdk:"secret"`
}

type appModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
	Plan types.String `tfsdk:"plan"`

	Region    types.String `tfsdk:"region"`
	ProjectID types.String `tfsdk:"project_id"`
	Tags      types.Set    `tfsdk:"tags"`
	Runtime   types.String `tfsdk:"runtime"`

	Image          types.String `tfsdk:"image"`
	Port           types.Int64  `tfsdk:"port"`
	HealthPath     types.String `tfsdk:"health_path"`
	StartCommand   types.String `tfsdk:"start_command"`
	ReplicasMin    types.Int64  `tfsdk:"replicas_min"`
	ReplicasMax    types.Int64  `tfsdk:"replicas_max"`
	AutoDeploy     types.Bool   `tfsdk:"auto_deploy"`
	WaitForBuild   types.Bool   `tfsdk:"wait_for_build"`
	SourceURL      types.String `tfsdk:"source_url"`
	GitRef         types.String `tfsdk:"git_ref"`
	GitConnection  types.String `tfsdk:"git_connection_id"`
	Builder        types.String `tfsdk:"builder"`
	Preset         types.String `tfsdk:"preset"`
	BuildCommand   types.String `tfsdk:"build_command"`
	InstallCommand types.String `tfsdk:"install_command"`
	OutputDir      types.String `tfsdk:"output_dir"`
	ContextDir     types.String `tfsdk:"context_dir"`
	DockerfilePath types.String `tfsdk:"dockerfile_path"`

	Env []appEnvModel `tfsdk:"env"`

	Code            types.String `tfsdk:"code"`
	URL             types.String `tfsdk:"url"`
	DesiredState    types.String `tfsdk:"desired_state"`
	ObservedState   types.String `tfsdk:"observed_state"`
	ReplicasDesired types.Int64  `tfsdk:"replicas_desired"`
	CreatedAt       types.String `tfsdk:"created_at"`
}

func (r *appResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_app"
}

func (r *appResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A container app: an image or a git repository, run and routed for you.\n\n" +
			"Two ways to start one, and they are exclusive. Give an `image` to run something that " +
			"is already built; give a `source_url` to have the platform build the repository, in " +
			"which case the first build is queued with the app and this resource waits for it.\n\n" +
			"**Starting and stopping an app is not modelled here.** It is an action rather than a " +
			"property, the same way a server's power state is: a plan should not decide to stop " +
			"something that is serving traffic.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The app's short code, which is also its identity everywhere else " +
					"in the API and the label in its default hostname.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Lowercase letters, digits and dashes, starting with a letter. " +
					"Renaming is applied in place; the `code` and the default hostname do not follow it.",
			},
			"plan": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "App plan slug. It decides the memory and CPU limits, the monthly " +
					"price and the replica ceiling. Changed in place, which restarts the containers.",
			},
			"region": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Region slug; defaults to the first active region when omitted. " +
					"There is no migration between regions, so a change replaces the app.\n\n" +
					"**Write-only.** An app's body does not report which region it landed in, so " +
					"this is never refreshed and drift in it cannot be detected. It is not marked " +
					"Computed for exactly that reason: a computed value nobody can fill would " +
					"stay unknown.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"project_id": projectAttribute("app"),
			"tags":       tagsAttribute("app"),
			"runtime": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Isolation runtime: `runc`, `runsc` (gVisor) or `kata` (microVM). " +
					"The last two need a node that registers them, so asking for one where none " +
					"exists is refused rather than downgraded.\n\n" +
					"Chosen when the container is placed and not changeable afterwards.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"image": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "An OCI image from an allowed public registry. Leave it out when " +
					"`source_url` is set: the build produces the image, and this then reports " +
					"whichever image the app is running.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"port": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The port the app listens on inside the container. Defaults to 8080.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"health_path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "An HTTP path a new version has to answer before it takes over " +
					"traffic. Empty means a plain TCP check on the app's port, which proves the " +
					"process is listening and nothing about whether it works.",
			},
			"start_command": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Overrides the image's own command, run through a shell. Leave it " +
					"out unless detection picked the wrong process.",
			},
			"replicas_min": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The fewest containers to keep running.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"replicas_max": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The most the platform may run, bounded by the plan. Equal to the " +
					"minimum pins the count.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"auto_deploy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether a push to the tracked branch builds and deploys by itself.\n\n" +
					"Leaving it ON means the running image can change without Terraform, which is " +
					"usually the point of connecting a repository. It is switched off automatically " +
					"when the source is unlinked.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"wait_for_build": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for a build it started to finish. With it off, " +
					"an apply that changes the source succeeds while the build is still running " +
					"and can still fail -- the app then keeps serving its previous image and the " +
					"plan reported nothing.",
			},

			"source_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "An HTTPS git repository to build. Removing it UNLINKS the app: it " +
					"keeps running its current image and auto-deploy is switched off with the link.",
			},
			"git_ref": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Branch or tag to build. Defaults to `main`.",
			},
			"git_connection_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "A connection from the account's git integrations, for a PRIVATE " +
					"repository. Every build then mints a short-lived credential from it. Omit it " +
					"for a public repository.",
			},
			"builder": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Who writes the Dockerfile. `dockerfile` (the default) means the " +
					"repository carries one; `preset` means the platform generates one for the " +
					"stack named in `preset`, which is how a repository with no Dockerfile deploys.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"preset": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Stack slug for the `preset` builder. Required with it, refused without it.",
			},
			"build_command": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Overrides the preset's build step.",
			},
			"install_command": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Overrides the preset's dependency install step.",
			},
			"output_dir": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The directory that matters at the end of the build: the document " +
					"root for `php`, the build output for `static`. Empty means the preset's default.",
			},
			"context_dir": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Repository subdirectory to build from, for a monorepo. Relative, " +
					"no leading slash.",
			},
			"dockerfile_path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Dockerfile path relative to the build directory. Defaults to " +
					"`Dockerfile`, and only applies to the `dockerfile` builder.",
			},

			"code": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"url": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The default hostname the app is served on.",
			},
			"desired_state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "What the platform is trying to run: `running`, `stopped` or " +
					"`suspended`. Changed through the panel's start and stop, not from here.",
			},
			"observed_state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "What the node last reported. It lags `desired_state` while a " +
					"deploy is rolling, and the gap between the two is what a deploy actually is.",
			},
			"replicas_desired": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "How many containers the platform currently wants to run.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
		Blocks: map[string]schema.Block{
			"env": schema.ListNestedBlock{
				MarkdownDescription: "Environment variables. The whole set is replaced on every change: " +
					"a variable absent here is removed.\n\n" +
					"**They are never read back.** The endpoint that returns decrypted values writes " +
					"an audit entry on every call, so refreshing them on each plan would fill the " +
					"account's audit log with reads nobody made deliberately. The consequence is " +
					"honest and worth knowing: a value changed in the panel is NOT detected as " +
					"drift, and the next apply that touches anything else will overwrite it with " +
					"whatever the configuration says.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"key": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "A shell-exportable name: letters, digits and underscores, " +
								"not starting with a digit.",
						},
						"value": schema.StringAttribute{
							Required:  true,
							Sensitive: true,
							MarkdownDescription: "The value. It is sealed at rest on the platform and it is " +
								"written to the TERRAFORM STATE FILE in the clear, like every other " +
								"attribute -- protect the state file accordingly.",
						},
						"phase": schema.StringAttribute{
							Optional: true,
							MarkdownDescription: "`run` (the default), `build`, or `both`. A `build` value " +
								"becomes an argument of the build command and is echoed into the build " +
								"log, which is why it is bounded and why it cannot also be secret.",
						},
						"secret": schema.BoolAttribute{
							Optional: true,
							MarkdownDescription: "Masks the value in the panel and omits it from exports.\n\n" +
								"**Not an encryption boundary.** The node receives every value in the " +
								"clear, because a container cannot start otherwise. Refused together " +
								"with a `build` phase.",
						},
					},
				},
			},
		},
	}
}

func (r *appResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan appModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := fbapi.AppCreateInputBody{
		Name: plan.Name.ValueString(),
		Plan: plan.Plan.ValueString(),
	}
	setIfConfigured(plan.Image, &body.Image)
	setIfConfigured(plan.Region, &body.Region)
	setIfConfigured(plan.ProjectID, &body.ProjectId)
	setIfConfigured(plan.HealthPath, &body.HealthPath)
	setIfConfigured(plan.StartCommand, &body.StartCommand)
	setIfConfigured(plan.GitRef, &body.GitRef)
	setIfConfigured(plan.GitConnection, &body.GitConnectionId)
	setIfConfigured(plan.Preset, &body.Preset)
	setIfConfigured(plan.BuildCommand, &body.BuildCommand)
	setIfConfigured(plan.InstallCommand, &body.InstallCommand)
	setIfConfigured(plan.OutputDir, &body.OutputDir)
	setIfConfigured(plan.ContextDir, &body.ContextDir)
	body.Tags = tagsFromPlan(ctx, plan.Tags, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	setIfConfigured(plan.DockerfilePath, &body.DockerfilePath)
	setIfConfigured(plan.SourceURL, &body.SourceUrl)
	if v := plan.Port.ValueInt64(); v > 0 && !plan.Port.IsUnknown() {
		body.Port = ptr(int32(v))
	}
	if v := plan.Builder.ValueString(); v != "" && !plan.Builder.IsUnknown() {
		b := fbapi.AppCreateInputBodyBuilder(v)
		body.Builder = &b
	}
	if v := plan.Runtime.ValueString(); v != "" && !plan.Runtime.IsUnknown() {
		rt := fbapi.AppCreateInputBodyRuntime(v)
		body.Runtime = &rt
	}
	// Env goes in the CREATE rather than in a second call. The build is queued
	// inside the same transaction as the app, so a build-phase variable set
	// afterwards would arrive after the build that needed it had started.
	if len(plan.Env) > 0 {
		vars := appEnvFrom(plan.Env)
		body.Env = &vars
	}

	out, err := r.client.API.AppCreateWithResponse(ctx, &fbapi.AppCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the app", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the app",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	code := out.JSON201.Code
	applyApp(ctx, &plan, out.JSON201, &resp.Diagnostics)
	// State before anything else. An app is billed from creation, so an
	// interrupted apply must not leave one that no state file names.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Scale and auto-deploy are their own endpoints and are not part of the
	// create, so a configuration that sets them is a create plus two edits.
	if !r.applyScale(ctx, code, &plan, nil, &resp.Diagnostics) {
		return
	}
	if !r.applyAutoDeploy(ctx, code, &plan, nil, &resp.Diagnostics) {
		return
	}

	if plan.WaitForBuild.ValueBool() && plan.SourceURL.ValueString() != "" {
		build, ok := r.latestBuild(ctx, code, &resp.Diagnostics)
		if !ok {
			return
		}
		if build != nil {
			if _, err := r.client.WaitForBuild(ctx, code, build.Id); err != nil {
				// The app EXISTS and is in state; only its first build failed.
				// Saying that plainly is the difference between "delete it and
				// try again" and "fix the Dockerfile".
				waitError(&resp.Diagnostics, "Waiting for the first build", err)
				return
			}
		}
	}
	if !r.refresh(ctx, code, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *appResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state appModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.AppGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the app", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the app",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyApp(ctx, &state, out.JSON200, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *appResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state appModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	code := state.ID.ValueString()

	if !applyGrouping(ctx, &resp.Diagnostics, groupingUpdate{
		Noun: "app", PlanTags: plan.Tags, StateTags: state.Tags,
		PlanProject: plan.ProjectID, StateProject: state.ProjectID,
		SetTags: func(ctx context.Context, b fbapi.TagsBody) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.AppTagsSetWithResponse(ctx, code, b)
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
		SetProject: func(ctx context.Context, pid *string) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.AppProjectSetWithResponse(ctx, code,
				fbapi.AppProjectSetJSONRequestBody{ProjectId: pid})
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
	}) {
		return
	}

	// Env first. A new variable is almost always a prerequisite of the code
	// that is about to be deployed, and setting it after the build would deploy
	// an image that cannot start.
	if !appEnvEqual(plan.Env, state.Env) {
		vars := appEnvFrom(plan.Env)
		out, err := r.client.API.AppEnvSetWithResponse(ctx, code,
			fbapi.AppEnvSetInputBody{Vars: &vars})
		if err != nil {
			apiError(&resp.Diagnostics, "Setting the app's environment", err)
			return
		}
		if out.StatusCode() >= 400 {
			settingRefused(&resp.Diagnostics, "Setting the app's environment",
				out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
			return
		}
	}

	if !plan.Name.Equal(state.Name) || !plan.Port.Equal(state.Port) ||
		!plan.HealthPath.Equal(state.HealthPath) || !plan.StartCommand.Equal(state.StartCommand) {
		// One endpoint for four fields, so it always sends all four. Reading
		// the planned values rather than only the changed ones is what keeps it
		// from blanking the other three.
		upd := fbapi.AppUpdateInputBody{
			Name: plan.Name.ValueString(),
			Port: int32(plan.Port.ValueInt64()),
		}
		setIfConfigured(plan.HealthPath, &upd.HealthPath)
		setIfConfigured(plan.StartCommand, &upd.StartCommand)
		out, err := r.client.API.AppUpdateWithResponse(ctx, code, upd)
		if err != nil {
			apiError(&resp.Diagnostics, "Updating the app", err)
			return
		}
		if out.JSON200 == nil {
			settingRefused(&resp.Diagnostics, "Updating the app",
				out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
			return
		}
	}

	if !plan.Plan.Equal(state.Plan) {
		out, err := r.client.API.AppResizeWithResponse(ctx, code,
			fbapi.AppResizeInputBody{Plan: plan.Plan.ValueString()})
		if err != nil {
			apiError(&resp.Diagnostics, "Resizing the app", err)
			return
		}
		if out.JSON200 == nil {
			settingRefused(&resp.Diagnostics, "Resizing the app",
				out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
			return
		}
	}

	if !r.applyScale(ctx, code, &plan, &state, &resp.Diagnostics) {
		return
	}

	// The source change goes LAST of the edits, because it is the one that
	// queues a build, and everything above is a prerequisite of that build
	// rather than a consequence of it.
	sourceChanged := !plan.SourceURL.Equal(state.SourceURL) ||
		!plan.GitRef.Equal(state.GitRef) ||
		!plan.GitConnection.Equal(state.GitConnection) ||
		!plan.Builder.Equal(state.Builder) ||
		!plan.Preset.Equal(state.Preset) ||
		!plan.BuildCommand.Equal(state.BuildCommand) ||
		!plan.InstallCommand.Equal(state.InstallCommand) ||
		!plan.OutputDir.Equal(state.OutputDir) ||
		!plan.ContextDir.Equal(state.ContextDir) ||
		!plan.DockerfilePath.Equal(state.DockerfilePath)

	var buildBefore string
	if sourceChanged && plan.WaitForBuild.ValueBool() {
		// Remembered BEFORE the change, because the source endpoint answers
		// with the app rather than with the build it queued. Comparing the
		// newest build id afterwards is the only way to name the one this
		// apply is responsible for.
		if b, ok := r.latestBuild(ctx, code, &resp.Diagnostics); ok && b != nil {
			buildBefore = b.Id
		} else if resp.Diagnostics.HasError() {
			return
		}
	}
	if sourceChanged {
		src := fbapi.AppSourceInputBody{SourceUrl: plan.SourceURL.ValueString()}
		setIfConfigured(plan.GitRef, &src.GitRef)
		setIfConfigured(plan.GitConnection, &src.GitConnectionId)
		setIfConfigured(plan.Preset, &src.Preset)
		setIfConfigured(plan.BuildCommand, &src.BuildCommand)
		setIfConfigured(plan.InstallCommand, &src.InstallCommand)
		setIfConfigured(plan.OutputDir, &src.OutputDir)
		setIfConfigured(plan.ContextDir, &src.ContextDir)
		setIfConfigured(plan.DockerfilePath, &src.DockerfilePath)
		if v := plan.Builder.ValueString(); v != "" && !plan.Builder.IsUnknown() {
			b := fbapi.AppSourceInputBodyBuilder(v)
			src.Builder = &b
		}
		out, err := r.client.API.AppSourceSetWithResponse(ctx, code, src)
		if err != nil {
			apiError(&resp.Diagnostics, "Setting the app's source", err)
			return
		}
		if out.JSON200 == nil {
			settingRefused(&resp.Diagnostics, "Setting the app's source",
				out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
			return
		}
	}

	// Auto-deploy AFTER the source, because unlinking a source switches it off
	// on the platform's side and an earlier write would be overwritten.
	if !r.applyAutoDeploy(ctx, code, &plan, &state, &resp.Diagnostics) {
		return
	}

	if sourceChanged && plan.WaitForBuild.ValueBool() {
		b, ok := r.latestBuild(ctx, code, &resp.Diagnostics)
		if !ok {
			return
		}
		if b != nil && b.Id != buildBefore {
			if _, err := r.client.WaitForBuild(ctx, code, b.Id); err != nil {
				waitError(&resp.Diagnostics, "Waiting for the build", err)
				return
			}
		}
	}

	if !r.refresh(ctx, code, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *appResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state appModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.AppDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the app", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		settingRefused(&resp.Diagnostics, "Deleting the app",
			out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
	}
}

// ImportState takes the app's code, which is what every app endpoint addresses
// it by.
func (r *appResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.AddWarning("Environment variables cannot be imported",
		"The provider does not read an app's environment back -- the endpoint that returns "+
			"decrypted values writes an audit entry on every call. An imported app therefore has "+
			"no `env` blocks in state, and the first apply will SET whatever the configuration "+
			"lists, removing anything it does not list.\n\n"+
			"Copy the current variables out of the panel into the configuration before applying.")
}

func (r *appResource) applyScale(ctx context.Context, code string, plan, state *appModel, diags *diagSink) bool {
	if plan.ReplicasMin.IsUnknown() && plan.ReplicasMax.IsUnknown() {
		return true
	}
	if state != nil && plan.ReplicasMin.Equal(state.ReplicasMin) && plan.ReplicasMax.Equal(state.ReplicasMax) {
		return true
	}
	if state == nil && plan.ReplicasMin.IsNull() && plan.ReplicasMax.IsNull() {
		return true
	}
	body := fbapi.AppScaleInputBody{
		ReplicasMin: int32(plan.ReplicasMin.ValueInt64()),
		ReplicasMax: int32(plan.ReplicasMax.ValueInt64()),
	}
	out, err := r.client.API.AppScaleWithResponse(ctx, code, body)
	if err != nil {
		apiError(diags, "Scaling the app", err)
		return false
	}
	if out.JSON200 == nil {
		settingRefused(diags, "Scaling the app",
			out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
		return false
	}
	return true
}

func (r *appResource) applyAutoDeploy(ctx context.Context, code string, plan, state *appModel, diags *diagSink) bool {
	if plan.AutoDeploy.IsUnknown() || plan.AutoDeploy.IsNull() {
		return true
	}
	if state != nil && plan.AutoDeploy.Equal(state.AutoDeploy) {
		return true
	}
	out, err := r.client.API.AppAutoDeploySetWithResponse(ctx, code,
		fbapi.AppAutoDeployInputBody{Enabled: plan.AutoDeploy.ValueBool()})
	if err != nil {
		apiError(diags, "Setting the app's auto-deploy", err)
		return false
	}
	if out.JSON200 == nil {
		settingRefused(diags, "Setting the app's auto-deploy",
			out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
		return false
	}
	return true
}

// latestBuild returns the newest build, or nil when the app has none. The list
// is newest-first, which is the API's own ordering rather than an assumption
// this provider imposes.
func (r *appResource) latestBuild(ctx context.Context, code string, diags *diagSink) (*fbapi.BuildBody, bool) {
	out, err := r.client.API.AppBuildsListWithResponse(ctx, code)
	if err != nil {
		apiError(diags, "Reading the app's builds", err)
		return nil, false
	}
	if out.JSON200 == nil {
		settingRefused(diags, "Reading the app's builds",
			out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
		return nil, false
	}
	if out.JSON200.Builds == nil || len(*out.JSON200.Builds) == 0 {
		return nil, true
	}
	builds := *out.JSON200.Builds
	return &builds[0], true
}

func (r *appResource) refresh(ctx context.Context, code string, m *appModel, diags *diagSink) bool {
	out, err := r.client.API.AppGetWithResponse(ctx, code)
	if err != nil {
		apiError(diags, "Re-reading the app", err)
		return false
	}
	if out.JSON200 == nil {
		settingRefused(diags, "Re-reading the app",
			out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse)
		return false
	}
	applyApp(ctx, m, out.JSON200, diags)
	return true
}

// applyApp fills the model from the API's answer.
//
// `env` is deliberately untouched: the API does return it, through an endpoint
// that audits every read, so the configured value stays in state and drift in it
// is invisible. That is stated on the attribute itself rather than only here.
func applyApp(ctx context.Context, m *appModel, b *fbapi.AppBody, diags *diagSink) {
	m.ID = types.StringValue(b.Code)
	m.Code = types.StringValue(b.Code)
	m.Name = types.StringValue(b.Name)
	m.Image = types.StringValue(b.Image)
	m.Port = types.Int64Value(int64(b.Port))
	m.Runtime = types.StringValue(b.Runtime)
	m.AutoDeploy = types.BoolValue(b.AutoDeploy)
	m.DesiredState = types.StringValue(b.DesiredState)
	m.ReplicasMin = types.Int64Value(int64(b.ReplicasMin))
	m.ReplicasMax = types.Int64Value(int64(b.ReplicasMax))
	m.ReplicasDesired = types.Int64Value(int64(b.ReplicasDesired))
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))

	m.URL = optString(b.Url)
	m.ObservedState = optString(b.ObservedState)
	m.HealthPath = optString(b.HealthPath)
	m.StartCommand = optString(b.StartCommand)
	m.SourceURL = optString(b.SourceUrl)
	m.GitRef = optString(b.GitRef)
	m.GitConnection = optString(b.GitConnectionId)
	m.Preset = optString(b.Preset)
	m.BuildCommand = optString(b.BuildCommand)
	m.InstallCommand = optString(b.InstallCommand)
	m.OutputDir = optString(b.OutputDir)
	m.ContextDir = optString(b.ContextDir)
	m.DockerfilePath = optString(b.DockerfilePath)
	m.ProjectID = optString(b.ProjectId)
	applyTags(ctx, &m.Tags, b.Tags, diags)
	// `plan` is Required, so a null in state after apply is an inconsistent
	// result rather than a refresh. The field is optional in the response, so
	// it is only taken when the API actually sent one.
	if b.Plan != nil && *b.Plan != "" {
		m.Plan = types.StringValue(*b.Plan)
	}
	// `builder` is Computed, and a Computed attribute left UNKNOWN cannot be
	// written to state at all -- null can. An app running a registry image has
	// no builder, and that is the null.
	if b.Builder != nil {
		m.Builder = types.StringValue(string(*b.Builder))
	} else {
		m.Builder = types.StringNull()
	}
}

func appEnvFrom(env []appEnvModel) []fbapi.EnvVarBody {
	out := make([]fbapi.EnvVarBody, 0, len(env))
	for _, e := range env {
		v := fbapi.EnvVarBody{Key: e.Key.ValueString(), Value: e.Value.ValueString()}
		if p := e.Phase.ValueString(); p != "" {
			ph := fbapi.EnvVarBodyPhase(p)
			v.Phase = &ph
		}
		if !e.Secret.IsNull() && !e.Secret.IsUnknown() {
			v.Secret = ptr(e.Secret.ValueBool())
		}
		out = append(out, v)
	}
	return out
}

func appEnvEqual(a, b []appEnvModel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Key.Equal(b[i].Key) || !a[i].Value.Equal(b[i].Value) ||
			!a[i].Phase.Equal(b[i].Phase) || !a[i].Secret.Equal(b[i].Secret) {
			return false
		}
	}
	return true
}
