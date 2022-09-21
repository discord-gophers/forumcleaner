package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/diamondburned/arikawa/v3/state"
	"github.com/diamondburned/arikawa/v3/utils/json/option"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
)

const (
	GCInterval    = time.Minute
	SolvedTimeout = time.Hour
	SolvedTag     = "solved"

	HerderRole = 370280974593818644
	SgtTailor  = 189020382559207425
)

func main() {
	log.SetFlags(0)

	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatalln("No $BOT_TOKEN given.")
	}

	var b Bot

	b.s = state.New("Bot " + token)
	b.s.AddInteractionHandler(&b)
	b.s.AddIntents(gateway.IntentGuilds)
	b.s.AddHandler(func(*gateway.ReadyEvent) {
		me, _ := b.s.Me()
		log.Println("connected to the gateway as", me.Tag())
	})

	if err := overwriteCommands(b.s); err != nil {
		log.Fatalln("cannot update commands:", err)
	}

	if err := b.s.Open(context.Background()); err != nil {
		log.Fatalln("cannot connect:", err)
	}

	go b.startGarbageCollection()

	if err := b.s.Wait(context.Background()); err != nil {
		log.Fatalln("discord gone bad:", err)
	}
}

var commands = []api.CreateCommandData{
	{
		Name:        "solved",
		Description: "marks the current forum post as resolved",
	},
}

func overwriteCommands(s *state.State) error {
	app, err := s.CurrentApplication()
	if err != nil {
		return errors.Wrap(err, "cannot get current app ID")
	}

	_, err = s.BulkOverwriteCommands(app.ID, commands)
	return err
}

type Bot struct {
	s *state.State
}

func (b *Bot) startGarbageCollection() {
	for range time.NewTicker(GCInterval).C {
		b.collectGarbage()
	}
}

func (b *Bot) collectGarbage() {
	log.Println("garbage collecting solved threads")
	guilds, err := b.s.Guilds()
	if err != nil {
		log.Println("error fetching guilds: ", err)
		return
	}

	for _, guild := range guilds {
		b.cleanGuild(guild)
	}
}

func (b *Bot) cleanGuild(guild discord.Guild) {
	log.Println("cleaning guild", guild.ID, guild.Name)
	threads, err := b.s.ActiveThreads(guild.ID)
	if err != nil {
		log.Println("error getting active threads", err)
		return
	}

	log.Printf("%d active threads\n", len(threads.Threads))

	// tag cache per guild. Per parent channelID (forum channel) we keep track
	// of the TagID. If an entry in this map exists it means we've already looked it up.
	// If a solved tag exists for the forum the map entry will be the TagID in question.
	// Nill indicates that no solved tag exists for this forum
	tagCache := make(map[discord.ChannelID]*discord.TagID)

	for _, thread := range threads.Threads {
		log.Println("checking thread", thread.ID, thread.Name)
		tagID, ok := tagCache[thread.ParentID]

		// cache hit and no solved tag exists
		if ok && tagID == nil {
			log.Println("skipping, no solved tag exists")
			continue
		}

		if !ok {
			tagID, err = b.getPostSolvedTag(thread)
			if err != nil {
				log.Println("unable to get solved tag for thread:", err)
				continue
			}
			tagCache[thread.ParentID] = tagID
		}

		// cache miss but no solved tag exists
		if tagID == nil {
			log.Println("skipping, no solved tag exists")
			continue
		}

		// no solved tag set
		if !slices.Contains(thread.AppliedTags, *tagID) {
			log.Println("skipping, solved tag not set")
			continue
		}

		// post hasn't expired yet
		if time.Since(thread.LastMessageID.Time()) < SolvedTimeout {
			log.Println("skipping, post has recent activity")
			continue
		}

		log.Println("closing thread")

		// post has the solved tag and had no activity. Time to archive
		err = b.s.Client.ModifyChannel(thread.ID, api.ModifyChannelData{
			Archived: option.True,
		})

		if err != nil {
			log.Println("Error closing thread: ", err)
		}
	}
}

func (b *Bot) getPostSolvedTag(post discord.Channel) (*discord.TagID, error) {
	parent, err := b.s.Client.Channel(post.ParentID)
	if err != nil {
		return nil, err
	}

	for _, tag := range parent.AvailableTags {
		if tag.Name == SolvedTag {
			return &tag.ID, nil
		}
	}
	return nil, nil
}

func (b *Bot) HandleInteraction(ev *discord.InteractionEvent) *api.InteractionResponse {
	switch data := ev.Data.(type) {
	case *discord.CommandInteraction:
		switch data.Name {
		case "solved":
			return b.cmdSolved(ev, data)
		default:
			return errorResponse(fmt.Sprintf("unknown command %q", data.Name))
		}
	default:
		return errorResponse(fmt.Sprintf("unknown interaction %T", ev.Data))
	}
}

func (b *Bot) checkAllowlist(g discord.GuildID, c *discord.Channel, m *discord.Member) bool {
	if m.User.ID == SgtTailor {
		return true
	}

	if c.OwnerID == m.User.ID {
		return true
	}

	return slices.Contains(m.RoleIDs, HerderRole)
}

func (b *Bot) cmdSolved(ev *discord.InteractionEvent, data *discord.CommandInteraction) *api.InteractionResponse {
	channel, err := b.s.Client.Channel(ev.ChannelID)
	if err != nil {
		return errorResponse(fmt.Sprintf("can't read channel: %s", err))
	}

	allowed := b.checkAllowlist(ev.GuildID, channel, ev.Member)
	if !allowed {
		return errorResponse("permission denied")
	}

	if !channel.ParentID.IsValid() {
		return errorResponse("this command only works in forum posts")
	}

	parent, err := b.s.Client.Channel(channel.ParentID)
	if err != nil {
		return errorResponse(fmt.Sprintf("can't read parent channel: %s", err))
	}

	if parent.Type != discord.GuildForum {
		return errorResponse("this command only works in forum posts")
	}

	var solvedTag *discord.Tag
	for _, tag := range parent.AvailableTags {
		if tag.Name == SolvedTag {
			solvedTag = &tag
			break
		}
	}

	if solvedTag == nil {
		return errorResponse("no solved tag found to apply")
	}

	if slices.Contains(channel.AppliedTags, solvedTag.ID) {
		return errorResponse("post already marked as solved")
	}

	newtags := append(channel.AppliedTags, solvedTag.ID)
	err = b.s.Client.ModifyChannel(ev.ChannelID, api.ModifyChannelData{
		AppliedTags: &newtags,
	})

	if err != nil {
		return errorResponse(fmt.Sprintf("error applying tag: %s", err))
	}

	return &api.InteractionResponse{
		Type: api.MessageInteractionWithSource,
		Data: &api.InteractionResponseData{
			Content:         option.NewNullableString("Thread marked as solved"),
			AllowedMentions: &api.AllowedMentions{},
		},
	}
}

func errorResponse(err string) *api.InteractionResponse {
	return &api.InteractionResponse{
		Type: api.MessageInteractionWithSource,
		Data: &api.InteractionResponseData{
			Content:         option.NewNullableString(err),
			Flags:           discord.EphemeralMessage,
			AllowedMentions: &api.AllowedMentions{ /* none */ },
		},
	}
}
